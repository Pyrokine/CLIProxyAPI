package pluginhost

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

type callbackContextRegistry struct {
	next        atomic.Uint64
	nextCleanup atomic.Uint64
	mu          sync.RWMutex
	contexts    map[string]callbackContextEntry
}

type callbackContextEntry struct {
	ctx      context.Context
	pluginID string
	instance *hostCallbackInstance
	cleanup  []callbackContextCleanup
}

type callbackContextCleanup struct {
	id uint64
	fn func()
}

func newCallbackContextRegistry() *callbackContextRegistry {
	return &callbackContextRegistry{contexts: make(map[string]callbackContextEntry)}
}

func (r *callbackContextRegistry) open(ctx context.Context, pluginID string, instance *hostCallbackInstance) (string, func()) {
	if r == nil {
		return "", func() {}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	pluginID = strings.TrimSpace(pluginID)
	if instance == nil {
		instance = hostCallbackInstanceFromContext(ctx)
	}
	ctx = withHostCallbackInstance(withHostCallbackPluginID(ctx, pluginID), instance)
	id := strconv.FormatUint(r.next.Add(1), 10)
	r.mu.Lock()
	r.contexts[id] = callbackContextEntry{ctx: ctx, pluginID: pluginID, instance: instance}
	r.mu.Unlock()

	var once sync.Once
	return id, func() {
		once.Do(func() {
			r.mu.Lock()
			entry := r.contexts[id]
			delete(r.contexts, id)
			r.mu.Unlock()
			for _, item := range entry.cleanup {
				if item.fn != nil {
					item.fn()
				}
			}
		})
	}
}

func (r *callbackContextRegistry) pluginID(id string) string {
	if r == nil || id == "" {
		return ""
	}
	r.mu.RLock()
	entry := r.contexts[id]
	r.mu.RUnlock()
	return strings.TrimSpace(entry.pluginID)
}

func (r *callbackContextRegistry) addCleanupHandle(id string, cleanup func()) (func(), bool) {
	if r == nil || strings.TrimSpace(id) == "" || cleanup == nil {
		return func() {}, false
	}
	id = strings.TrimSpace(id)
	cleanupID := r.nextCleanup.Add(1)
	r.mu.Lock()
	entry, ok := r.contexts[id]
	if ok {
		entry.cleanup = append(entry.cleanup, callbackContextCleanup{id: cleanupID, fn: cleanup})
		r.contexts[id] = entry
	}
	r.mu.Unlock()
	if !ok {
		cleanup()
		return func() {}, false
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			entry, exists := r.contexts[id]
			if exists {
				for index, item := range entry.cleanup {
					if item.id == cleanupID {
						entry.cleanup = append(entry.cleanup[:index], entry.cleanup[index+1:]...)
						r.contexts[id] = entry
						break
					}
				}
			}
			r.mu.Unlock()
		})
	}, true
}

func (r *callbackContextRegistry) lookup(id string) (context.Context, string, *hostCallbackInstance, bool) {
	if r == nil || strings.TrimSpace(id) == "" {
		return nil, "", nil, false
	}
	r.mu.RLock()
	entry, ok := r.contexts[strings.TrimSpace(id)]
	r.mu.RUnlock()
	if !ok || entry.ctx == nil {
		return nil, "", nil, false
	}
	return entry.ctx, strings.TrimSpace(entry.pluginID), entry.instance, true
}

func (h *Host) openCallbackContext(ctx context.Context) (string, func()) {
	return h.openCallbackContextForPlugin(ctx, "")
}

func (h *Host) openCallbackContextForPlugin(ctx context.Context, pluginID string) (string, func()) {
	return h.openCallbackContextForPluginInstance(ctx, pluginID, nil)
}

func (h *Host) openCallbackContextForPluginInstance(ctx context.Context, pluginID string, instance *hostCallbackInstance) (string, func()) {
	if h == nil || h.callbackContexts == nil {
		return "", func() {}
	}
	return h.callbackContexts.open(ctx, pluginID, instance)
}

func (h *Host) addCallbackCleanupHandle(id string, cleanup func()) (func(), bool) {
	if h == nil || h.callbackContexts == nil {
		if id != "" && cleanup != nil {
			cleanup()
		}
		return func() {}, false
	}
	return h.callbackContexts.addCleanupHandle(id, cleanup)
}

func (h *Host) callbackContextPluginID(id string) string {
	if h == nil || h.callbackContexts == nil {
		return ""
	}
	return h.callbackContexts.pluginID(id)
}

func (h *Host) activeCallbackContext(id string) (context.Context, string, bool) {
	ctx, pluginID, _, ok := h.lookupCallbackContext(id)
	return ctx, pluginID, ok
}

func (h *Host) lookupCallbackContext(id string) (context.Context, string, *hostCallbackInstance, bool) {
	if h == nil || h.callbackContexts == nil {
		return nil, "", nil, false
	}
	return h.callbackContexts.lookup(id)
}

func (h *Host) resolvePluginCallbackContext(ctx context.Context, callbackID string) (context.Context, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(callbackID) == "" {
		return ctx, nil
	}
	callbackCtx, pluginID, exists := h.activeCallbackContext(callbackID)
	if !exists {
		return nil, fmt.Errorf("host callback ID is not open")
	}
	if callerID := hostCallbackPluginIDFromContext(ctx); callerID != "" && callerID != pluginID {
		return nil, fmt.Errorf("host callback ID does not belong to the calling plugin")
	}
	return callbackCtx, nil
}
