package translator

import (
	"context"
	"sync"
)

var pluginHooksRegistry struct {
	mu    sync.RWMutex
	hooks PluginHooks
}

type pluginHooksContextKey struct{}

type pluginHooksContextValue struct {
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

// WithPluginHooks binds translator hooks to one request. A nil hook is retained
// explicitly so the request cannot inherit another service's compatibility hook.
func WithPluginHooks(ctx context.Context, hooks PluginHooks) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, pluginHooksContextKey{}, pluginHooksContextValue{hooks: hooks})
}

// HasPluginHooks reports whether translation hooks apply to this request.
// An explicit nil request value overrides package-level hooks.
func HasPluginHooks(ctx context.Context) bool {
	return pluginHooksForContext(ctx) != nil
}

func pluginHooksForContext(ctx context.Context) PluginHooks {
	if ctx != nil {
		if value, ok := ctx.Value(pluginHooksContextKey{}).(pluginHooksContextValue); ok {
			return value.hooks
		}
	}
	return currentPluginHooks()
}
