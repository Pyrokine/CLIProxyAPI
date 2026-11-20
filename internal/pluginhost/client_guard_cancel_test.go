package pluginhost

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type waitingGuardClient struct {
	started  chan struct{}
	release  <-chan struct{}
	shutdown chan struct{}
	once     sync.Once
}

func (client *waitingGuardClient) Call(context.Context, string, []byte) ([]byte, error) {
	close(client.started)
	<-client.release
	return marshalRPCResult(rpcEmptyResponse{})
}

func (client *waitingGuardClient) Shutdown() {
	client.once.Do(func() { close(client.shutdown) })
}

func TestGuardedPluginCallCancellationKeepsNativeLeaseUntilReturn(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	native := &waitingGuardClient{started: make(chan struct{}), release: release, shutdown: make(chan struct{})}
	client := newGuardedPluginClient(native).(*guardedPluginClient)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := callPlugin[rpcEmptyResponse](ctx, client, "fixture.wait", rpcEmptyResponse{})
		result <- err
	}()
	select {
	case <-native.started:
	case <-time.After(time.Second):
		t.Fatal("plugin call did not start")
	}
	cancel()
	returned := false
	select {
	case err := <-result:
		returned = true
		if !errors.Is(err, context.Canceled) {
			t.Errorf("plugin call error = %v, want cancellation", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Error("plugin caller remained blocked after cancellation")
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer shutdownCancel()
	if err := client.shutdownContext(shutdownCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("shutdown of active native call = %v, want deadline", err)
	}
	select {
	case <-native.shutdown:
		t.Error("native library shut down before its call returned")
	default:
	}
	unblock()
	if !returned {
		<-result
	}
	select {
	case <-native.shutdown:
	case <-time.After(time.Second):
		t.Fatal("native library was not released after its call returned")
	}
}

type panickingGuardClient struct{}

func (panickingGuardClient) Call(context.Context, string, []byte) ([]byte, error) {
	panic("fixture plugin panic")
}

func (panickingGuardClient) Shutdown() {}

func TestGuardedPluginCallPreservesPanicRecovery(t *testing.T) {
	client := newGuardedPluginClient(panickingGuardClient{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_, _ = client.Call(ctx, "fixture.panic", nil)
	}()
	if recovered != "fixture plugin panic" {
		t.Fatalf("caller recovered %v, want original plugin panic", recovered)
	}
	client.Shutdown()
}
