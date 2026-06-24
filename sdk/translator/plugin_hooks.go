package translator

import (
	"context"
	"sync"
)

var pluginHooksRegistry struct {
	mu    sync.RWMutex
	hooks PluginHooks
}

// PluginHooks defines optional translator extension hooks provided by plugins.
type PluginHooks interface {
	NormalizeRequest(ctx context.Context, from, to Format, model string, body []byte, stream bool) []byte
	TranslateRequest(ctx context.Context, from, to Format, model string, body []byte, stream bool) ([]byte, bool)
	NormalizeResponseBefore(
		ctx context.Context,
		from, to Format,
		model string,
		originalRequestRawJSON, requestRawJSON, body []byte,
		stream bool,
	) []byte
	TranslateResponse(
		ctx context.Context,
		from, to Format,
		model string,
		originalRequestRawJSON, requestRawJSON, body []byte,
		stream bool,
	) ([]byte, bool)
	NormalizeResponseAfter(
		ctx context.Context,
		from, to Format,
		model string,
		originalRequestRawJSON, requestRawJSON, body []byte,
		stream bool,
	) []byte
}

// SetPluginHooks registers package-level translator hooks supplied by the plugin host.
func SetPluginHooks(hooks PluginHooks) {
	pluginHooksRegistry.mu.Lock()
	pluginHooksRegistry.hooks = hooks
	pluginHooksRegistry.mu.Unlock()
}

func currentPluginHooks() PluginHooks {
	pluginHooksRegistry.mu.RLock()
	defer pluginHooksRegistry.mu.RUnlock()
	return pluginHooksRegistry.hooks
}
