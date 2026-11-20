package helps

import (
	"bytes"
	"context"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

// EmitWebSocketResponseEvent delivers one raw upstream WebSocket response frame.
func EmitWebSocketResponseEvent(
	ctx context.Context,
	opts cliproxyexecutor.Options,
	auth *cliproxyauth.Auth,
	provider,
	model string,
	payload []byte,
) {
	if len(payload) == 0 {
		return
	}
	if strings.EqualFold(strings.TrimSpace(provider), "codex") {
		if headers := parseCodexQuotaEventHeaders(payload); len(headers) > 0 {
			logging.MergeResponseHeaders(ctx, headers)
			cliproxyauth.RecordUpstreamResponseHeaders(ctx, headers)
		}
	}
	if opts.WebSocketResponseObserver == nil {
		return
	}
	var authID, authLabel, authType string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, _ = auth.AccountInfo()
	}
	var requestID, traceID string
	if opts.Metadata != nil {
		requestID, _ = opts.Metadata["request_id"].(string)
		traceID, _ = opts.Metadata["trace_id"].(string)
	}
	opts.WebSocketResponseObserver(
		ctx, cliproxyexecutor.WebSocketResponseEvent{
			RequestID:      strings.TrimSpace(requestID),
			TraceID:        strings.TrimSpace(traceID),
			SourceFormat:   opts.SourceFormat.String(),
			Model:          model,
			RequestedModel: requestedWebSocketModel(opts, model),
			Provider:       provider,
			AuthID:         authID,
			AuthLabel:      authLabel,
			AuthType:       authType,
			EventType:      gjson.GetBytes(payload, "type").String(),
			Payload:        bytes.Clone(payload),
			Metadata:       opts.Metadata,
		},
	)
}

func requestedWebSocketModel(opts cliproxyexecutor.Options, fallback string) string {
	fallback = strings.TrimSpace(fallback)
	if len(opts.Metadata) == 0 {
		return fallback
	}
	switch value := opts.Metadata[cliproxyexecutor.RequestedModelMetadataKey].(type) {
	case string:
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	case []byte:
		if value := strings.TrimSpace(string(value)); value != "" {
			return value
		}
	}
	return fallback
}
