package grokbuild

import (
	"bytes"
	"context"
	"net/http"
	"slices"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

var keepaliveSSEComment = []byte(": keepalive\n\n")

// ModelInfo contains the model metadata used by Grok Shell.
type ModelInfo struct {
	ID              string
	DisplayName     string
	ContextLength   int
	ReasoningLevels []string
}

// reasoningEffort is one Grok Shell reasoning option.
type reasoningEffort struct {
	Value string `json:"value"`
}

// modelEntry is the Grok Shell representation of one model.
type modelEntry struct {
	ID               string            `json:"id"`
	Model            string            `json:"model"`
	Name             string            `json:"name"`
	ContextWindow    int               `json:"context_window,omitempty"`
	APIBackend       string            `json:"api_backend"`
	SupportedInAPI   bool              `json:"supported_in_api"`
	ReasoningEfforts []reasoningEffort `json:"reasoning_efforts,omitempty"`
}

// Response is the Grok Shell model list envelope.
type Response struct {
	Object string       `json:"object"`
	Data   []modelEntry `json:"data"`
}

// IsGrokShellUserAgent reports whether userAgent belongs to a Grok CLI client.
func IsGrokShellUserAgent(userAgent string) bool {
	return IsGrokClientUserAgent(userAgent)
}

// IsGrokClientUserAgent reports whether userAgent belongs to Grok Shell or Grok Pager.
func IsGrokClientUserAgent(userAgent string) bool {
	normalized := strings.ToLower(userAgent)
	return strings.Contains(normalized, "grok-shell") || strings.Contains(normalized, "grok-pager")
}

// IsGrokClientHeaders reports whether headers identify a Grok CLI client.
func IsGrokClientHeaders(headers http.Header) bool {
	for name, values := range headers {
		if strings.EqualFold(name, "User-Agent") && slices.ContainsFunc(values, IsGrokClientUserAgent) {
			return true
		}
	}
	return false
}

// IsGrokClientContext checks request headers from context before the explicit headers.
func IsGrokClientContext(ctx context.Context, headers http.Header) bool {
	if ctx != nil {
		if ginCtx, ok := util.GinContextValue(ctx).(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
			if IsGrokClientHeaders(ginCtx.Request.Header) {
				return true
			}
		}
	}
	return IsGrokClientHeaders(headers)
}

// TransformKeepaliveSSEFrame converts a Grok keepalive event to an SSE comment.
func TransformKeepaliveSSEFrame(frame []byte, isGrokClient bool) ([]byte, bool) {
	if !isGrokClient || !IsKeepaliveSSEFrame(frame) {
		return frame, false
	}
	return bytes.Clone(keepaliveSSEComment), true
}

// IsKeepaliveSSEFrame reports whether an SSE frame carries a keepalive event.
func IsKeepaliveSSEFrame(frame []byte) bool {
	for _, line := range bytes.Split(frame, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		switch {
		case bytes.HasPrefix(line, []byte("event:")):
			if bytes.Equal(bytes.TrimSpace(line[len("event:"):]), []byte("keepalive")) {
				return true
			}
		case bytes.HasPrefix(line, []byte("data:")):
			payload := bytes.TrimSpace(line[len("data:"):])
			if strings.EqualFold(strings.TrimSpace(gjson.GetBytes(payload, "type").String()), "keepalive") {
				return true
			}
		}
	}
	return false
}

// BuildResponse formats model metadata for Grok Shell.
func BuildResponse(models []ModelInfo) Response {
	entries := make([]modelEntry, 0, len(models))
	for _, model := range models {
		name := strings.TrimSpace(model.DisplayName)
		if name == "" {
			name = model.ID
		}
		entry := modelEntry{
			ID:             model.ID,
			Model:          model.ID,
			Name:           name,
			ContextWindow:  model.ContextLength,
			APIBackend:     "responses",
			SupportedInAPI: true,
		}
		for _, level := range model.ReasoningLevels {
			if level = strings.TrimSpace(level); level != "" {
				entry.ReasoningEfforts = append(entry.ReasoningEfforts, reasoningEffort{Value: level})
			}
		}
		entries = append(entries, entry)
	}
	return Response{Object: "list", Data: entries}
}
