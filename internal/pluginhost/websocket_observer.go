package pluginhost

import (
	"bytes"
	"context"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
	log "github.com/sirupsen/logrus"
)

func (h *Host) HasWebSocketResponseObservers() bool {
	if h == nil {
		return false
	}
	for _, record := range h.activeRecords() {
		if h.isPluginFused(record.id) {
			continue
		}
		if record.plugin.Capabilities.WebSocketResponseObserver != nil {
			return true
		}
	}
	return false
}

func (h *Host) ObserveWebSocketResponseEvent(ctx context.Context, event pluginapi.WebSocketResponseEvent) {
	h.ObserveWebSocketResponseEventExcept(ctx, event, "")
}

func (h *Host) ObserveWebSocketResponseEventExcept(
	ctx context.Context,
	event pluginapi.WebSocketResponseEvent,
	skipPluginID string,
) {
	if h == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	} else {
		ctx = context.WithoutCancel(ctx)
	}
	skipPluginID = strings.TrimSpace(skipPluginID)
	metadata := sanitizeWebSocketMetadata(event.Metadata)
	for _, record := range h.activeRecords() {
		observer := record.plugin.Capabilities.WebSocketResponseObserver
		if observer == nil || record.id == skipPluginID || h.isPluginFused(record.id) || !h.recordCurrent(record) {
			continue
		}
		next := event
		next.Payload = bytes.Clone(event.Payload)
		next.Metadata = cloneInterceptorMetadata(metadata)
		h.callWebSocketResponseObserver(ctx, record, observer, next)
	}
}

// OpenWebSocketResponseCallback returns a request-scoped callback and an idempotent close function.
// The callback becomes a no-op after close or request cancellation.
func (h *Host) OpenWebSocketResponseCallback(
	ctx context.Context,
	skipPluginID string,
) (pluginapi.WebSocketResponseCallback, func()) {
	if h == nil || !h.HasWebSocketResponseObservers() {
		return nil, func() {}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	callbackID, closeCallback := h.openCallbackContextForPlugin(ctx, strings.TrimSpace(skipPluginID))
	stopOnRequestEnd := context.AfterFunc(ctx, closeCallback)
	closeRequestCallback := func() {
		stopOnRequestEnd()
		closeCallback()
	}
	callback := func(event pluginapi.WebSocketResponseEvent) {
		callbackCtx, initiatingPluginID, active := h.activeCallbackContext(callbackID)
		if !active || callbackCtx.Err() != nil {
			return
		}
		h.ObserveWebSocketResponseEventExcept(callbackCtx, event, initiatingPluginID)
	}
	return callback, closeRequestCallback
}

func (h *Host) callWebSocketResponseObserver(
	ctx context.Context,
	record capabilityRecord,
	observer pluginapi.WebSocketResponseObserver,
	event pluginapi.WebSocketResponseEvent,
) {
	if h == nil || observer == nil || h.isPluginFused(record.id) || !h.recordCurrent(record) {
		return
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			h.fusePlugin(record.id, "WebSocketResponseObserver.ObserveWebSocketResponseEvent", recovered)
		}
	}()
	if errObserve := observer.ObserveWebSocketResponseEvent(ctx, event); errObserve != nil {
		log.WithError(errObserve).WithField("plugin_id", record.id).Warn(
			"pluginhost: websocket response observer failed",
		)
	}
}

func sanitizeWebSocketMetadata(src map[string]any) map[string]any {
	return removeSensitiveWebSocketMetadata(sanitizePluginMetadata(src))
}

func removeSensitiveWebSocketMetadata(src map[string]any) map[string]any {
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]any, len(src))
	for key, value := range src {
		if util.IsSensitiveFieldName(key) {
			continue
		}
		out[key] = removeSensitiveWebSocketMetadataValue(value)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func removeSensitiveWebSocketMetadataValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return removeSensitiveWebSocketMetadata(typed)
	case []any:
		out := make([]any, len(typed))
		for index, item := range typed {
			out[index] = removeSensitiveWebSocketMetadataValue(item)
		}
		return out
	default:
		return typed
	}
}
