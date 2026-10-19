package pluginhost

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
)

type modelStreamBridge struct {
	next    atomic.Uint64
	mu      sync.Mutex
	streams map[string]modelStreamEntry
}

type modelStreamEntry struct {
	pluginID   string
	instanceID uintptr
	ctx        context.Context
	chunks     <-chan handlers.ModelExecutionChunk
	cancel     context.CancelFunc
	onClose    func()
}

func newModelStreamBridge() *modelStreamBridge {
	return &modelStreamBridge{streams: make(map[string]modelStreamEntry)}
}

func modelStreamOwner(ctx context.Context) (string, uintptr) {
	instanceID := hostCallbackInstanceIDFromContext(ctx)
	if instance := hostCallbackInstanceFromContext(ctx); instance != nil {
		instanceID = instance.id
	}
	return hostCallbackPluginIDFromContext(ctx), instanceID
}

func (b *modelStreamBridge) open(
	ctx context.Context,
	chunks <-chan handlers.ModelExecutionChunk,
	cancel context.CancelFunc,
) string {
	if ctx == nil {
		ctx = context.Background()
	}
	if b == nil || chunks == nil || ctx.Err() != nil {
		if cancel != nil {
			cancel()
		}
		return ""
	}
	pluginID, instanceID := modelStreamOwner(ctx)
	id := strconv.FormatUint(b.next.Add(1), 10)
	b.mu.Lock()
	if instance := hostCallbackInstanceFromContext(ctx); instance != nil && instance.closed.Load() {
		b.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		return ""
	}
	b.streams[id] = modelStreamEntry{pluginID: pluginID, instanceID: instanceID, ctx: ctx, chunks: chunks, cancel: cancel}
	b.mu.Unlock()
	stop := context.AfterFunc(ctx, func() { _ = b.close(ctx, id) })
	b.setCleanup(ctx, id, func() { stop() })
	return id
}

func (b *modelStreamBridge) setCleanup(ctx context.Context, id string, cleanup func()) {
	if b == nil || cleanup == nil {
		return
	}
	pluginID, instanceID := modelStreamOwner(ctx)
	b.mu.Lock()
	entry, ok := b.streams[id]
	if ok && entry.pluginID == pluginID && entry.instanceID == instanceID {
		previous := entry.onClose
		entry.onClose = func() {
			if previous != nil {
				previous()
			}
			cleanup()
		}
		b.streams[id] = entry
		b.mu.Unlock()
		return
	}
	b.mu.Unlock()
	cleanup()
}

func (b *modelStreamBridge) read(ctx context.Context, id string) (handlers.ModelExecutionChunk, bool, error) {
	if b == nil {
		return handlers.ModelExecutionChunk{}, true, fmt.Errorf("model stream bridge is unavailable")
	}
	if id == "" {
		return handlers.ModelExecutionChunk{}, true, fmt.Errorf("model stream id is required")
	}
	b.mu.Lock()
	entry, ok := b.streams[id]
	b.mu.Unlock()
	if !ok || entry.chunks == nil {
		return handlers.ModelExecutionChunk{}, true, fmt.Errorf("model stream %s is not open", id)
	}
	pluginID, instanceID := modelStreamOwner(ctx)
	if entry.pluginID != pluginID || entry.instanceID != instanceID {
		return handlers.ModelExecutionChunk{}, true, fmt.Errorf("model stream does not belong to the calling plugin instance")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := entry.ctx.Err(); err != nil {
		_ = b.close(ctx, id)
		return handlers.ModelExecutionChunk{}, true, err
	}
	select {
	case <-ctx.Done():
		_ = b.close(ctx, id)
		return handlers.ModelExecutionChunk{}, true, ctx.Err()
	case <-entry.ctx.Done():
		_ = b.close(ctx, id)
		return handlers.ModelExecutionChunk{}, true, entry.ctx.Err()
	case chunk, okRead := <-entry.chunks:
		if !okRead {
			_ = b.close(ctx, id)
			return handlers.ModelExecutionChunk{}, true, nil
		}
		if chunk.Err != nil {
			_ = b.close(ctx, id)
			return chunk, true, nil
		}
		return chunk, false, nil
	}
}

func (b *modelStreamBridge) close(ctx context.Context, id string) error {
	if b == nil || id == "" {
		return nil
	}
	pluginID, instanceID := modelStreamOwner(ctx)
	b.mu.Lock()
	entry, exists := b.streams[id]
	if exists && (entry.pluginID != pluginID || entry.instanceID != instanceID) {
		b.mu.Unlock()
		return fmt.Errorf("model stream does not belong to the calling plugin instance")
	}
	delete(b.streams, id)
	b.mu.Unlock()
	closeModelStreamEntry(entry)
	return nil
}

func (b *modelStreamBridge) closeInstance(pluginID string, instanceID uintptr) {
	if b == nil {
		return
	}
	b.mu.Lock()
	var entries []modelStreamEntry
	for id, entry := range b.streams {
		if entry.pluginID == pluginID && entry.instanceID == instanceID {
			delete(b.streams, id)
			entries = append(entries, entry)
		}
	}
	b.mu.Unlock()
	for _, entry := range entries {
		closeModelStreamEntry(entry)
	}
}

func closeModelStreamEntry(entry modelStreamEntry) {
	if entry.cancel != nil {
		entry.cancel()
	}
	if entry.onClose != nil {
		entry.onClose()
	}
}
