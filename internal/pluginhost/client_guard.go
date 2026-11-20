package pluginhost

import (
	"context"
	"fmt"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const pluginShutdownTimeout = 15 * time.Second

type guardedPluginClient struct {
	mu              sync.Mutex
	inner           pluginClient
	instance        *hostCallbackInstance
	calls           int
	closed          bool
	shutdownStarted bool
	shutdownDone    chan struct{}
}

func newGuardedPluginClient(inner pluginClient) pluginClient {
	instance := pluginCallbackInstance(inner)
	if instance == nil {
		instance = &hostCallbackInstance{}
	}
	return &guardedPluginClient{
		inner:        inner,
		instance:     instance,
		shutdownDone: make(chan struct{}),
	}
}

func pluginCallbackInstance(client pluginClient) *hostCallbackInstance {
	if provider, ok := client.(interface{ callbackInstance() *hostCallbackInstance }); ok {
		return provider.callbackInstance()
	}
	return nil
}

func (c *guardedPluginClient) callbackInstance() *hostCallbackInstance {
	if c == nil {
		return nil
	}
	return c.instance
}

func (c *guardedPluginClient) Call(ctx context.Context, method string, request []byte) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	inner, errAcquire := c.acquire()
	if errAcquire != nil {
		return nil, errAcquire
	}
	if ctx.Done() == nil {
		defer c.release()
		return inner.Call(ctx, method, request)
	}

	type callResult struct {
		response []byte
		err      error
		panicked any
	}
	finished := make(chan callResult, 1)
	go func() {
		var result callResult
		defer func() {
			result.panicked = recover()
			c.release()
			if result.panicked != nil {
				log.WithField("method", method).Error("pluginhost: plugin RPC call panicked")
			}
			finished <- result
		}()
		if result.err = ctx.Err(); result.err != nil {
			return
		}
		result.response, result.err = inner.Call(ctx, method, request)
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result := <-finished:
		if result.panicked != nil {
			panic(result.panicked)
		}
		return result.response, result.err
	}
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

func (c *guardedPluginClient) cancelHostHTTPResources() {
	if c == nil {
		return
	}
	c.instance.closed.Store(true)
	c.mu.Lock()
	inner := c.inner
	c.mu.Unlock()
	if scoped, ok := inner.(interface{ cancelHostHTTPResources() }); ok {
		scoped.cancelHostHTTPResources()
	}
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
