package cliproxy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/redisqueue"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
)

func TestShutdownContinuesAfterCallerTimeoutAndSharesFinalResult(t *testing.T) {
	service := &Service{}
	service.authQueueWorkers.Add(1)
	firstCtx, cancelFirst := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancelFirst()
	if errShutdown := service.Shutdown(firstCtx); !errors.Is(errShutdown, context.DeadlineExceeded) {
		t.Fatalf("first Shutdown() error = %v, want deadline exceeded", errShutdown)
	}
	service.authQueueWorkers.Done()
	if errShutdown := service.Shutdown(context.Background()); errShutdown != nil {
		t.Fatalf("second Shutdown() error = %v", errShutdown)
	}
	if errShutdown := service.Shutdown(context.Background()); errShutdown != nil {
		t.Fatalf("third Shutdown() error = %v", errShutdown)
	}
}

func TestShutdownJoinsAllResourceErrors(t *testing.T) {
	service := &Service{
		watcher: &WatcherWrapper{
			stop: func() error {
				return coreauth.ErrAuthDurableStateUncertain
			},
		},
	}
	service.pluginRuntimeRetryWG.Add(1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	errShutdown := service.shutdown(ctx)
	service.pluginRuntimeRetryWG.Done()
	if !errors.Is(errShutdown, context.DeadlineExceeded) {
		t.Fatalf("shutdown() error = %v, want deadline exceeded", errShutdown)
	}
	if !errors.Is(errShutdown, coreauth.ErrAuthDurableStateUncertain) {
		t.Fatalf("shutdown() error = %v, want durable-state uncertainty", errShutdown)
	}
}

func TestServiceShutdownKeepsUsageQueueDisabled(t *testing.T) {
	queue := redisqueue.NewQueue()
	queue.SetEnabled(true)
	service := &Service{
		cfg:        &config.Config{},
		usageQueue: queue,
	}
	if errShutdown := service.shutdown(context.Background()); errShutdown != nil {
		t.Fatalf("shutdown() error = %v", errShutdown)
	}
	if !service.shuttingDown.Load() {
		t.Fatal("service shutdown state was not published")
	}
	if queue.Enabled() {
		t.Fatal("usage queue remained enabled after shutdown")
	}
	if errApply := service.applyConfigUpdateRuntime(
		context.Background(), &config.Config{Home: config.HomeConfig{Enabled: true}},
	); errApply == nil {
		t.Fatal("config update succeeded after shutdown")
	}
	if queue.Enabled() {
		t.Fatal("post-shutdown config update re-enabled the usage queue")
	}
}

func TestNewShutdownContextReturnsFreshContext(t *testing.T) {
	originalTimeout := serviceShutdownTimeout
	serviceShutdownTimeout = 20 * time.Millisecond
	defer func() {
		serviceShutdownTimeout = originalTimeout
	}()

	service := &Service{}
	ctx1, cancel1 := service.newShutdownContext()
	defer cancel1()
	<-ctx1.Done()

	ctx2, cancel2 := service.newShutdownContext()
	defer cancel2()
	if err := ctx2.Err(); err != nil {
		t.Fatalf("expected fresh shutdown context, got %v", err)
	}

	deadline1, ok1 := ctx1.Deadline()
	deadline2, ok2 := ctx2.Deadline()
	if !ok1 || !ok2 {
		t.Fatal("expected both shutdown contexts to have deadlines")
	}
	if !deadline2.After(deadline1) {
		t.Fatalf("expected second deadline after first, got first=%v second=%v", deadline1, deadline2)
	}
}
