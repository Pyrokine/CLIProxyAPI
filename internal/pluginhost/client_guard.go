package pluginhost

import (
	"context"
	"fmt"
	"sync"
	"time"
)

const pluginShutdownTimeout = 15 * time.Second

type guardedPluginClient struct {
	mu              sync.Mutex
	inner           pluginClient
	calls           int
	closed          bool
	shutdownStarted bool
	shutdownDone    chan struct{}
}

func newGuardedPluginClient(inner pluginClient) pluginClient {
	return &guardedPluginClient{
		inner:        inner,
		shutdownDone: make(chan struct{}),
	}
}

func (c *guardedPluginClient) Call(ctx context.Context, method string, request []byte) ([]byte, error) {
	inner, errAcquire := c.acquire()
	if errAcquire != nil {
		return nil, errAcquire
	}
	defer c.release()
	return inner.Call(ctx, method, request)
}

func (c *guardedPluginClient) acquire() (pluginClient, error) {
	if c == nil {
		return nil, fmt.Errorf("plugin client is closed")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.inner == nil {
		return nil, fmt.Errorf("plugin client is closed")
	}
	c.calls++
	return c.inner, nil
}

func (c *guardedPluginClient) release() {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.calls > 0 {
		c.calls--
	}
	inner := c.detachShutdownLocked()
	c.mu.Unlock()
	c.startShutdown(inner)
}

func (c *guardedPluginClient) Shutdown() {
	_ = c.shutdownContext(context.Background())
}

func (c *guardedPluginClient) shutdownContext(ctx context.Context) error {
	if c == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	c.closed = true
	inner := c.detachShutdownLocked()
	done := c.shutdownDone
	c.mu.Unlock()
	c.startShutdown(inner)

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("shutdown plugin client: %w", ctx.Err())
	}
}

func (c *guardedPluginClient) detachShutdownLocked() pluginClient {
	if c.shutdownStarted || !c.closed || c.calls > 0 {
		return nil
	}
	c.shutdownStarted = true
	inner := c.inner
	c.inner = nil
	if inner == nil {
		close(c.shutdownDone)
	}
	return inner
}

func (c *guardedPluginClient) startShutdown(inner pluginClient) {
	if inner == nil {
		return
	}
	go func() {
		defer close(c.shutdownDone)
		inner.Shutdown()
	}()
}

func shutdownPluginClient(ctx context.Context, client pluginClient) error {
	if client == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if contextual, ok := client.(interface {
		shutdownContext(context.Context) error
	}); ok {
		return contextual.shutdownContext(ctx)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		client.Shutdown()
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("shutdown plugin client: %w", ctx.Err())
	}
}

func boundedPluginShutdownContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	if _, hasDeadline := parent.Deadline(); hasDeadline {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, pluginShutdownTimeout)
}
