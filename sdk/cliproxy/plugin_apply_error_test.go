package cliproxy

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
)

type serviceUsageCapture struct {
	records chan string
}

func (p *serviceUsageCapture) HandleUsage(_ context.Context, record coreusage.Record) {
	p.records <- record.Model
}

func TestBuilderCreatesServiceOwnedUsageState(t *testing.T) {
	build := func() *Service {
		cfg := &config.Config{AuthDir: t.TempDir()}
		cfg.Plugins.Dir = t.TempDir()
		service, errBuild := NewBuilder().
			WithConfig(cfg).
			WithConfigPath(filepath.Join(t.TempDir(), "config.yaml")).
			Build()
		if errBuild != nil {
			t.Fatalf("Build() error = %v", errBuild)
		}
		return service
	}

	first := build()
	second := build()
	if first.usageManager == nil || second.usageManager == nil || first.usageManager == second.usageManager {
		t.Fatalf("usage managers are not service-owned: first=%p second=%p", first.usageManager, second.usageManager)
	}
	if first.usageStats == nil || second.usageStats == nil || first.usageStats == second.usageStats {
		t.Fatalf("usage statistics are not service-owned: first=%p second=%p", first.usageStats, second.usageStats)
	}
}

func TestServiceShutdownKeepsPeerUsageManagerRunning(t *testing.T) {
	build := func() *Service {
		cfg := &config.Config{AuthDir: t.TempDir()}
		cfg.Plugins.Dir = t.TempDir()
		service, errBuild := NewBuilder().
			WithConfig(cfg).
			WithConfigPath(filepath.Join(t.TempDir(), "config.yaml")).
			Build()
		if errBuild != nil {
			t.Fatalf("Build() error = %v", errBuild)
		}
		return service
	}

	first := build()
	second := build()
	firstCapture := &serviceUsageCapture{records: make(chan string, 2)}
	secondCapture := &serviceUsageCapture{records: make(chan string, 2)}
	first.RegisterUsagePlugin(firstCapture)
	second.RegisterUsagePlugin(secondCapture)
	first.usageManager.Start(context.Background())
	second.usageManager.Start(context.Background())
	first.usageQueue.SetEnabled(true)
	second.usageQueue.SetEnabled(true)
	firstMessages, unsubscribeFirst := first.usageQueue.SubscribeUsage()
	defer unsubscribeFirst()
	secondMessages, unsubscribeSecond := second.usageQueue.SubscribeUsage()
	defer unsubscribeSecond()
	<-firstMessages
	<-secondMessages

	if err := first.usageManager.Publish(
		context.Background(), coreusage.Record{Model: "first-before-stop"},
	); err != nil {
		t.Fatalf("first Publish() error = %v", err)
	}
	if err := second.usageManager.Publish(
		context.Background(), coreusage.Record{Model: "second-before-stop"},
	); err != nil {
		t.Fatalf("second Publish() error = %v", err)
	}
	requireServiceUsageRecord(t, firstCapture.records, "first-before-stop")
	requireServiceUsageRecord(t, secondCapture.records, "second-before-stop")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := first.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("first Shutdown() error = %v", err)
	}
	if err := first.usageManager.Publish(
		context.Background(), coreusage.Record{Model: "first-after-stop"},
	); !errors.Is(err, coreusage.ErrManagerStopped) {
		t.Fatalf("first Publish() after shutdown error = %v, want ErrManagerStopped", err)
	}
	select {
	case _, open := <-firstMessages:
		if open {
			t.Fatal("first usage subscriber remained open after shutdown")
		}
	case <-time.After(time.Second):
		t.Fatal("first usage subscriber was not closed by shutdown")
	}
	select {
	case _, open := <-secondMessages:
		if !open {
			t.Fatal("peer usage subscriber closed after first service shutdown")
		}
	default:
	}
	if err := second.usageManager.Publish(
		context.Background(), coreusage.Record{Model: "second-after-peer-stop"},
	); err != nil {
		t.Fatalf("second Publish() after peer shutdown error = %v", err)
	}
	requireServiceUsageRecord(t, secondCapture.records, "second-after-peer-stop")

	if err := second.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("second Shutdown() error = %v", err)
	}
}

func requireServiceUsageRecord(t *testing.T, records <-chan string, want string) {
	t.Helper()
	select {
	case got := <-records:
		if got != want {
			t.Fatalf("usage record = %q, want %q", got, want)
		}
	case <-time.After(time.Second):
		t.Fatalf("timeout waiting for usage record %q", want)
	}
}

func TestBuilderBuildReturnsPluginApplyError(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "plugin-root-file")
	if errWrite := os.WriteFile(blocker, []byte("not a directory"), 0o600); errWrite != nil {
		t.Fatalf("write plugin root blocker: %v", errWrite)
	}
	cfg := &config.Config{AuthDir: t.TempDir()}
	cfg.Plugins.Dir = blocker

	service, errBuild := NewBuilder().
		WithConfig(cfg).
		WithConfigPath(filepath.Join(t.TempDir(), "config.yaml")).
		Build()
	if errBuild == nil || service != nil {
		t.Fatalf("Build() = (%v, %v), want plugin apply error", service, errBuild)
	}
	if !strings.Contains(errBuild.Error(), "apply plugin config during build") {
		t.Fatalf("Build() error = %v", errBuild)
	}
}

func TestServiceRunDoesNotEnterStartedPhaseAfterListenerFailure(t *testing.T) {
	listener, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatal(errListen)
	}
	defer func() { _ = listener.Close() }()
	port := listener.Addr().(*net.TCPAddr).Port

	cfg := &config.Config{Host: "127.0.0.1", Port: port, AuthDir: t.TempDir()}
	cfg.Plugins.Dir = t.TempDir()
	afterStartCalled := false
	service, errBuild := NewBuilder().
		WithConfig(cfg).
		WithConfigPath(filepath.Join(t.TempDir(), "config.yaml")).
		WithHooks(Hooks{OnAfterStart: func(*Service) { afterStartCalled = true }}).
		Build()
	if errBuild != nil {
		t.Fatalf("Build() error = %v", errBuild)
	}

	errRun := service.Run(context.Background())
	if errRun == nil || !strings.Contains(errRun.Error(), "failed to start HTTP server") {
		t.Fatalf("Run() error = %v, want listener startup error", errRun)
	}
	if afterStartCalled {
		t.Fatal("Run() invoked OnAfterStart after listener startup failed")
	}
}

func TestServiceRunDoesNotStartServerAfterPluginApplyError(t *testing.T) {
	cfg := &config.Config{Host: "127.0.0.1", Port: 0, AuthDir: t.TempDir()}
	cfg.Plugins.Dir = t.TempDir()
	beforeStartCalled := false
	service, errBuild := NewBuilder().
		WithConfig(cfg).
		WithConfigPath(filepath.Join(t.TempDir(), "config.yaml")).
		WithHooks(Hooks{OnBeforeStart: func(*config.Config) { beforeStartCalled = true }}).
		Build()
	if errBuild != nil {
		t.Fatalf("Build() error = %v", errBuild)
	}

	blocker := filepath.Join(t.TempDir(), "plugin-root-file")
	if errWrite := os.WriteFile(blocker, []byte("not a directory"), 0o600); errWrite != nil {
		t.Fatalf("write plugin root blocker: %v", errWrite)
	}
	cfg.Plugins.Dir = blocker

	errRun := service.Run(context.Background())
	if errRun == nil || !strings.Contains(errRun.Error(), "apply plugin runtime config") {
		t.Fatalf("Run() error = %v, want plugin apply error", errRun)
	}
	if beforeStartCalled {
		t.Fatal("Run() invoked OnBeforeStart after plugin apply failed")
	}
	if service.serverErr != nil {
		t.Fatal("Run() launched the HTTP server after plugin apply failed")
	}
}
