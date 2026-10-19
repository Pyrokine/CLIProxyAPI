package pluginhost

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
)

type hostHTTPStreamKey struct {
	pluginID   string
	instanceID uintptr
	streamID   string
}

type hostHTTPStreamBridge struct {
	next    atomic.Uint64
	mu      sync.Mutex
	streams map[hostHTTPStreamKey]hostHTTPStreamEntry
}

type hostHTTPStreamEntry struct {
	chunks  <-chan pluginapi.HTTPStreamChunk
	cancel  context.CancelFunc
	onClose func()
}

func newHostHTTPStreamBridge() *hostHTTPStreamBridge {
	return &hostHTTPStreamBridge{streams: make(map[hostHTTPStreamKey]hostHTTPStreamEntry)}
}

func (b *hostHTTPStreamBridge) open(
	ctx context.Context,
	pluginID string,
	instanceID uintptr,
	chunks <-chan pluginapi.HTTPStreamChunk,
	cancel context.CancelFunc,
	onClose func(),
) string {
	if b == nil || chunks == nil {
		if cancel != nil {
			cancel()
		}
		if onClose != nil {
			onClose()
		}
		return ""
	}
	id := strconv.FormatUint(b.next.Add(1), 10)
	key := hostHTTPStreamKey{pluginID: strings.TrimSpace(pluginID), instanceID: instanceID, streamID: id}
	b.mu.Lock()
	b.streams[key] = hostHTTPStreamEntry{chunks: chunks, cancel: cancel, onClose: onClose}
	b.mu.Unlock()
	if ctx != nil {
		context.AfterFunc(ctx, func() { b.close(pluginID, instanceID, id) })
	}
	return id
}

func (b *hostHTTPStreamBridge) read(
	ctx context.Context,
	pluginID string,
	instanceID uintptr,
	id string,
) (pluginapi.HTTPStreamChunk, bool, error) {
	if b == nil || strings.TrimSpace(id) == "" {
		return pluginapi.HTTPStreamChunk{}, true, fmt.Errorf("http stream id is required")
	}
	key := hostHTTPStreamKey{pluginID: strings.TrimSpace(pluginID), instanceID: instanceID, streamID: strings.TrimSpace(id)}
	b.mu.Lock()
	entry := b.streams[key]
	b.mu.Unlock()
	if entry.chunks == nil {
		return pluginapi.HTTPStreamChunk{}, true, fmt.Errorf("http stream %s is not open", id)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		b.close(pluginID, instanceID, id)
		return pluginapi.HTTPStreamChunk{}, true, ctx.Err()
	case chunk, ok := <-entry.chunks:
		if !ok {
			b.close(pluginID, instanceID, id)
			return pluginapi.HTTPStreamChunk{}, true, nil
		}
		if chunk.Err != nil {
			b.close(pluginID, instanceID, id)
			return chunk, true, nil
		}
		return chunk, false, nil
	}
}

func (b *hostHTTPStreamBridge) close(pluginID string, instanceID uintptr, id string) {
	if b == nil || strings.TrimSpace(id) == "" {
		return
	}
	key := hostHTTPStreamKey{pluginID: strings.TrimSpace(pluginID), instanceID: instanceID, streamID: strings.TrimSpace(id)}
	b.mu.Lock()
	entry := b.streams[key]
	delete(b.streams, key)
	b.mu.Unlock()
	closeHostHTTPStreamEntry(entry)
}

func (b *hostHTTPStreamBridge) closeInstance(pluginID string, instanceID uintptr) {
	b.closeMatching(func(key hostHTTPStreamKey) bool {
		return key.pluginID == strings.TrimSpace(pluginID) && key.instanceID == instanceID
	})
}

func (b *hostHTTPStreamBridge) closePlugin(pluginID string) {
	b.closeMatching(func(key hostHTTPStreamKey) bool {
		return key.pluginID == strings.TrimSpace(pluginID)
	})
}

func (b *hostHTTPStreamBridge) closeAll() {
	b.closeMatching(func(hostHTTPStreamKey) bool { return true })
}

func (b *hostHTTPStreamBridge) closeMatching(matches func(hostHTTPStreamKey) bool) {
	if b == nil {
		return
	}
	b.mu.Lock()
	var entries []hostHTTPStreamEntry
	for key, entry := range b.streams {
		if matches(key) {
			delete(b.streams, key)
			entries = append(entries, entry)
		}
	}
	b.mu.Unlock()
	for _, entry := range entries {
		closeHostHTTPStreamEntry(entry)
	}
}

func closeHostHTTPStreamEntry(entry hostHTTPStreamEntry) {
	if entry.cancel != nil {
		entry.cancel()
	}
	if entry.onClose != nil {
		entry.onClose()
	}
}
