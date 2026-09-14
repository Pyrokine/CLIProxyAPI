package grokbuild

import "strings"

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

// IsGrokShellUserAgent reports whether userAgent belongs to Grok Shell.
func IsGrokShellUserAgent(userAgent string) bool {
	return strings.Contains(strings.ToLower(userAgent), "grok-shell")
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
