package pluginhost

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

type hostHTTPOperationKey struct {
	pluginID    string
	operationID string
}

type hostHTTPOperation struct {
	ctx              context.Context
	cancel           context.CancelFunc
	instance         *hostCallbackInstance
	callbackID       string
	started          bool
	stopCancel       func() bool
	stopScopeCleanup func()
	cleanupStream    func()
}

type hostHTTPOperationHandle struct {
	ctx         context.Context
	finish      func()
	pluginID    string
	instance    *hostCallbackInstance
	operationID string
	entry       *hostHTTPOperation
}

type hostHTTPOperationBridge struct {
	next       atomic.Uint64
	mu         sync.Mutex
	operations map[hostHTTPOperationKey]*hostHTTPOperation
}

func newHostHTTPOperationBridge() *hostHTTPOperationBridge {
	return &hostHTTPOperationBridge{operations: make(map[hostHTTPOperationKey]*hostHTTPOperation)}
}

func (b *hostHTTPOperationBridge) open(
	pluginID string, instance *hostCallbackInstance, callbackID string, parent context.Context, started bool,
) (string, *hostHTTPOperation, bool) {
	if b == nil {
		return "", nil, false
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	key := hostHTTPOperationKey{pluginID: strings.TrimSpace(pluginID), operationID: strconv.FormatUint(b.next.Add(1), 10)}
	entry := &hostHTTPOperation{ctx: ctx, cancel: cancel, instance: instance, callbackID: strings.TrimSpace(callbackID), started: started}
	b.mu.Lock()
	if ctx.Err() != nil || instance != nil && instance.closed.Load() {
		b.mu.Unlock()
		cancel()
		return "", nil, false
	}
	b.operations[key] = entry
	entry.stopCancel = context.AfterFunc(ctx, func() {
		b.cancel(key.pluginID, instance, key.operationID)
	})
	b.mu.Unlock()
	return key.operationID, entry, true
}

func (b *hostHTTPOperationBridge) claim(
	pluginID string, instance *hostCallbackInstance, operationID, callbackID string,
) (*hostHTTPOperation, bool) {
	if b == nil || strings.TrimSpace(operationID) == "" {
		return nil, false
	}
	key := hostHTTPOperationKey{pluginID: strings.TrimSpace(pluginID), operationID: strings.TrimSpace(operationID)}
	b.mu.Lock()
	defer b.mu.Unlock()
	entry := b.operations[key]
	if entry == nil || entry.started || entry.instance != instance || entry.callbackID != strings.TrimSpace(callbackID) || entry.ctx.Err() != nil {
		return nil, false
	}
	entry.started = true
	return entry, true
}

func (b *hostHTTPOperationBridge) setScopeCleanup(
	pluginID, operationID string, entry *hostHTTPOperation, stop func(),
) bool {
	if b == nil || entry == nil || stop == nil {
		if stop != nil {
			stop()
		}
		return false
	}
	key := hostHTTPOperationKey{pluginID: strings.TrimSpace(pluginID), operationID: operationID}
	b.mu.Lock()
	if b.operations[key] == entry && entry.stopScopeCleanup == nil && entry.ctx.Err() == nil {
		entry.stopScopeCleanup = stop
		b.mu.Unlock()
		return true
	}
	b.mu.Unlock()
	stop()
	return false
}

func (b *hostHTTPOperationBridge) setStreamCleanup(
	pluginID, operationID string, entry *hostHTTPOperation, cleanup func(),
) bool {
	if b == nil || entry == nil || cleanup == nil {
		if cleanup != nil {
			cleanup()
		}
		return false
	}
	key := hostHTTPOperationKey{pluginID: strings.TrimSpace(pluginID), operationID: operationID}
	b.mu.Lock()
	if b.operations[key] == entry && entry.cleanupStream == nil && entry.ctx.Err() == nil {
		entry.cleanupStream = cleanup
		b.mu.Unlock()
		return true
	}
	b.mu.Unlock()
	cleanup()
	return false
}

func (b *hostHTTPOperationBridge) finish(pluginID, operationID string, entry *hostHTTPOperation) {
	if b == nil || entry == nil {
		return
	}
	key := hostHTTPOperationKey{pluginID: strings.TrimSpace(pluginID), operationID: operationID}
	b.mu.Lock()
	if b.operations[key] == entry {
		delete(b.operations, key)
	}
	b.mu.Unlock()
	if entry.stopCancel != nil {
		entry.stopCancel()
	}
	if entry.stopScopeCleanup != nil {
		entry.stopScopeCleanup()
	}
	entry.cancel()
}

func (b *hostHTTPOperationBridge) cancel(pluginID string, instance *hostCallbackInstance, operationID string) {
	if b == nil || strings.TrimSpace(operationID) == "" {
		return
	}
	key := hostHTTPOperationKey{pluginID: strings.TrimSpace(pluginID), operationID: strings.TrimSpace(operationID)}
	b.mu.Lock()
	entry := b.operations[key]
	if entry != nil && entry.instance == instance {
		delete(b.operations, key)
	} else {
		entry = nil
	}
	b.mu.Unlock()
	cancelHostHTTPOperation(entry)
}

func (b *hostHTTPOperationBridge) closeInstance(pluginID string, instance *hostCallbackInstance) {
	if instance == nil {
		return
	}
	instance.closed.Store(true)
	b.cancelMatching(func(key hostHTTPOperationKey, entry *hostHTTPOperation) bool {
		return key.pluginID == strings.TrimSpace(pluginID) && entry.instance == instance
	})
}

func (b *hostHTTPOperationBridge) cancelPlugin(pluginID string) {
	if strings.TrimSpace(pluginID) == "" {
		return
	}
	b.cancelMatching(func(key hostHTTPOperationKey, _ *hostHTTPOperation) bool {
		return key.pluginID == strings.TrimSpace(pluginID)
	})
}

func (b *hostHTTPOperationBridge) cancelAll() {
	b.cancelMatching(func(_ hostHTTPOperationKey, entry *hostHTTPOperation) bool {
		if entry.instance != nil {
			entry.instance.closed.Store(true)
		}
		return true
	})
}

func (b *hostHTTPOperationBridge) cancelMatching(matches func(hostHTTPOperationKey, *hostHTTPOperation) bool) {
	if b == nil {
		return
	}
	b.mu.Lock()
	var entries []*hostHTTPOperation
	for key, entry := range b.operations {
		if matches(key, entry) {
			delete(b.operations, key)
			entries = append(entries, entry)
		}
	}
	b.mu.Unlock()
	for _, entry := range entries {
		cancelHostHTTPOperation(entry)
	}
}

func cancelHostHTTPOperation(entry *hostHTTPOperation) {
	if entry == nil {
		return
	}
	if entry.stopCancel != nil {
		entry.stopCancel()
	}
	if entry.stopScopeCleanup != nil {
		entry.stopScopeCleanup()
	}
	entry.cancel()
	if entry.cleanupStream != nil {
		entry.cleanupStream()
	}
}

func (h *Host) hostHTTPCallbackOwner(
	ctx context.Context, callbackID string,
) (context.Context, string, *hostCallbackInstance, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	pluginID := hostCallbackPluginIDFromContext(ctx)
	instance := hostCallbackInstanceFromContext(ctx)
	callbackID = strings.TrimSpace(callbackID)
	if callbackID == "" {
		return ctx, pluginID, instance, nil
	}
	parent, callbackPluginID, callbackInstance, exists := h.lookupCallbackContext(callbackID)
	if !exists {
		return nil, "", nil, fmt.Errorf("host callback ID is not open")
	}
	if pluginID != "" && pluginID != callbackPluginID {
		return nil, "", nil, fmt.Errorf("host callback ID does not belong to the calling plugin")
	}
	if instance != nil && instance != callbackInstance {
		return nil, "", nil, fmt.Errorf("host callback ID does not belong to the calling plugin instance")
	}
	return parent, callbackPluginID, callbackInstance, nil
}

func (h *Host) createHostHTTPOperation(
	ctx context.Context, callbackID string, claimed bool,
) (string, *hostHTTPOperation, string, *hostCallbackInstance, error) {
	if h == nil || h.httpOperations == nil {
		return "", nil, "", nil, fmt.Errorf("host http operation bridge is unavailable")
	}
	parent, pluginID, instance, errOwner := h.hostHTTPCallbackOwner(ctx, callbackID)
	if errOwner != nil {
		return "", nil, "", nil, errOwner
	}
	operationID, entry, opened := h.httpOperations.open(pluginID, instance, callbackID, parent, claimed)
	if !opened {
		return "", nil, "", nil, fmt.Errorf("host http operation cannot be opened for closed context or instance")
	}
	if strings.TrimSpace(callbackID) != "" {
		stopCleanup, attached := h.addCallbackCleanupHandle(callbackID, func() {
			h.httpOperations.cancel(pluginID, instance, operationID)
		})
		if !attached || !h.httpOperations.setScopeCleanup(pluginID, operationID, entry, stopCleanup) {
			h.httpOperations.cancel(pluginID, instance, operationID)
			return "", nil, "", nil, fmt.Errorf("host callback context closed while opening HTTP operation")
		}
	}
	return operationID, entry, pluginID, instance, nil
}

func (h *Host) acquireHostHTTPOperation(
	ctx context.Context, callbackID, operationID string,
) (hostHTTPOperationHandle, error) {
	if h == nil || h.httpOperations == nil {
		return hostHTTPOperationHandle{}, fmt.Errorf("host http operation bridge is unavailable")
	}
	operationID = strings.TrimSpace(operationID)
	var entry *hostHTTPOperation
	var pluginID string
	var instance *hostCallbackInstance
	if operationID == "" {
		var errOpen error
		operationID, entry, pluginID, instance, errOpen = h.createHostHTTPOperation(ctx, callbackID, true)
		if errOpen != nil {
			return hostHTTPOperationHandle{}, errOpen
		}
	} else {
		_, resolvedPluginID, resolvedInstance, errOwner := h.hostHTTPCallbackOwner(ctx, callbackID)
		if errOwner != nil {
			return hostHTTPOperationHandle{}, errOwner
		}
		pluginID, instance = resolvedPluginID, resolvedInstance
		var claimed bool
		entry, claimed = h.httpOperations.claim(pluginID, instance, operationID, callbackID)
		if !claimed {
			return hostHTTPOperationHandle{}, fmt.Errorf("host http operation %q is not open", operationID)
		}
	}
	return hostHTTPOperationHandle{
		ctx: entry.ctx, pluginID: pluginID, instance: instance, operationID: operationID, entry: entry,
		finish: func() { h.httpOperations.finish(pluginID, operationID, entry) },
	}, nil
}
