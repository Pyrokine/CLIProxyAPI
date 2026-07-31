package pluginhost

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type blockingPluginClient struct {
	entered      chan struct{}
	release      chan struct{}
	shutdownDone chan struct{}
	enterOnce    sync.Once
	shutdownOnce sync.Once
}

func (c *blockingPluginClient) Call(context.Context, string, []byte) ([]byte, error) {
	c.enterOnce.Do(func() { close(c.entered) })
	<-c.release
	return nil, nil
}

func (c *blockingPluginClient) Shutdown() {
	c.shutdownOnce.Do(func() { close(c.shutdownDone) })
}

func TestHostShutdownAllContextReturnsWhilePluginCallStalls(t *testing.T) {
	inner := &blockingPluginClient{
		entered:      make(chan struct{}),
		release:      make(chan struct{}),
		shutdownDone: make(chan struct{}),
	}
	client := newGuardedPluginClient(inner)
	host := New()
	host.loaded["blocking"] = &loadedPlugin{id: "blocking", client: client}
	callDone := make(chan struct{})
	go func() {
		_, _ = client.Call(context.Background(), "models_for_auth", nil)
		close(callDone)
	}()
	select {
	case <-inner.entered:
	case <-time.After(time.Second):
		t.Fatal("plugin call did not start")
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelShutdown()
	if errShutdown := host.ShutdownAllContext(shutdownCtx); !errors.Is(errShutdown, context.DeadlineExceeded) {
		t.Fatalf("ShutdownAllContext() error = %v, want deadline exceeded", errShutdown)
	}
	if !host.PluginBusy("blocking") {
		t.Fatal("pending plugin shutdown was no longer tracked")
	}
	if snapshot := host.Snapshot(); snapshot.enabled || len(snapshot.records) != 0 {
		t.Fatalf("plugin remained routable after bounded shutdown: %#v", snapshot)
	}
	select {
	case <-inner.shutdownDone:
		t.Fatal("plugin module was released while a call was active")
	default:
	}

	close(inner.release)
	select {
	case <-callDone:
	case <-time.After(time.Second):
		t.Fatal("plugin call did not finish")
	}
	select {
	case <-inner.shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("plugin module was not released after the call finished")
	}
	deadline := time.Now().Add(time.Second)
	for host.PluginBusy("blocking") {
		if time.Now().After(deadline) {
			t.Fatal("completed plugin shutdown remained tracked")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestGuardedPluginClientShutdownContextDefersRelease(t *testing.T) {
	inner := &blockingPluginClient{
		entered:      make(chan struct{}),
		release:      make(chan struct{}),
		shutdownDone: make(chan struct{}),
	}
	client := newGuardedPluginClient(inner)
	callDone := make(chan struct{})
	go func() {
		_, _ = client.Call(context.Background(), "models_for_auth", nil)
		close(callDone)
	}()
	select {
	case <-inner.entered:
	case <-time.After(time.Second):
		t.Fatal("plugin call did not start")
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelShutdown()
	contextual := client.(interface {
		ShutdownContext(context.Context) error
	})
	if errShutdown := contextual.ShutdownContext(shutdownCtx); !errors.Is(errShutdown, context.DeadlineExceeded) {
		t.Fatalf("ShutdownContext() error = %v, want deadline exceeded", errShutdown)
	}
	if _, errCall := client.Call(context.Background(), "models_for_auth", nil); errCall == nil {
		t.Fatal("closed plugin client accepted a new call")
	}
	select {
	case <-inner.shutdownDone:
		t.Fatal("plugin module was released while a call was active")
	default:
	}

	close(inner.release)
	select {
	case <-callDone:
	case <-time.After(time.Second):
		t.Fatal("plugin call did not finish")
	}
	select {
	case <-inner.shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("plugin module was not released after the call finished")
	}
	waitCtx, cancelWait := context.WithTimeout(context.Background(), time.Second)
	defer cancelWait()
	if errShutdown := contextual.ShutdownContext(waitCtx); errShutdown != nil {
		t.Fatalf("second ShutdownContext() error = %v", errShutdown)
	}
}
