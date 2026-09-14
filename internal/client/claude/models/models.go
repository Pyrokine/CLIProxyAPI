// Package models builds model catalogs for Anthropic clients.
package models

import (
	"encoding/base64"
	"maps"
	"sort"
	"strings"
)

const (
	clientModelPrefix          = "claude-fable-5-dd-"
	externalClientModelPrefix  = clientModelPrefix + "x-"
	collisionClientModelPrefix = clientModelPrefix + "c-"
)

// Catalog owns a deterministic Anthropic model response and its request mapping.
type Catalog struct {
	response map[string]any
	resolved map[string]string
}

// NewCatalog builds a catalog without mutating availableModels.
func NewCatalog(availableModels []map[string]any, disableCloaking bool) *Catalog {
	models := make([]map[string]any, 0, len(availableModels))
	resolved := make(map[string]string, len(availableModels)*2)
	providerIDs := make(map[string]struct{}, len(availableModels))
	for _, model := range availableModels {
		if providerID, ok := model["id"].(string); ok && providerID != "" {
			providerIDs[providerID] = struct{}{}
		}
	}
	for _, model := range availableModels {
		cloned := cloneModel(model)
		providerID, _ := cloned["id"].(string)
		if providerID != "" {
			clientID := ClientModelID(providerID)
			if disableCloaking {
				cloned["id"] = providerID
				resolved[providerID] = providerID
				if _, collidesWithProvider := providerIDs[clientID]; !collidesWithProvider {
					resolved[clientID] = providerID
				}
			} else {
				cloned["id"] = clientID
				resolved[clientID] = providerID
			}
		}
		models = append(models, cloned)
	}

	sort.SliceStable(
		models, func(i, j int) bool {
			displayNameI := stringValue(models[i], "display_name")
			displayNameJ := stringValue(models[j], "display_name")
			if displayNameI != displayNameJ {
				return displayNameI < displayNameJ
			}
			return stringValue(models[i], "id") < stringValue(models[j], "id")
		},
	)

	firstID := ""
	lastID := ""
	if len(models) > 0 {
		firstID = stringValue(models[0], "id")
		lastID = stringValue(models[len(models)-1], "id")
	}
	return &Catalog{
		response: map[string]any{
			"data":     models,
			"has_more": false,
			"first_id": firstID,
			"last_id":  lastID,
		},
		resolved: resolved,
	}
}

// Response returns the Anthropic models response owned by the catalog.
func (c *Catalog) Response() map[string]any {
	if c == nil {
		return emptyResponse()
	}
	return c.response
}

// ResolveModelID maps a client model ID back to the provider-facing ID.
func (c *Catalog) ResolveModelID(id string) string {
	if c == nil || id == "" {
		return id
	}
	base, suffix, hasSuffix := splitThinkingSuffix(id)
	resolved := c.resolved[base]
	if resolved == "" {
		resolved = resolveClientModelID(base)
	}
	if resolved == base {
		return id
	}
	if hasSuffix {
		return resolved + "(" + suffix + ")"
	}
	return resolved
}

// ClientModelID returns the collision-safe ID exposed to Anthropic clients.
func ClientModelID(id string) string {
	if id == "" {
		return id
	}
	if strings.HasPrefix(id, clientModelPrefix) {
		return collisionClientModelPrefix + encodeModelID(id)
	}
	if strings.HasPrefix(id, "claude-") {
		return id
	}
	return externalClientModelPrefix + encodeModelID(id)
}

// resolveClientModelID reverses ClientModelID without requiring a Catalog instance.
func resolveClientModelID(id string) string {
	if id == "" {
		return id
	}
	base, suffix, hasSuffix := splitThinkingSuffix(id)
	encoded := ""
	switch {
	case strings.HasPrefix(base, externalClientModelPrefix):
		encoded = strings.TrimPrefix(base, externalClientModelPrefix)
	case strings.HasPrefix(base, collisionClientModelPrefix):
		encoded = strings.TrimPrefix(base, collisionClientModelPrefix)
	default:
		return id
	}
	if encoded == "" {
		return id
	}
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(decoded) == 0 {
		return id
	}
	resolved := string(decoded)
	if ClientModelID(resolved) != base {
		return id
	}
	if hasSuffix {
		return resolved + "(" + suffix + ")"
	}
	return resolved
}

func emptyResponse() map[string]any {
	return map[string]any{
		"data":     []map[string]any{},
		"has_more": false,
		"first_id": "",
		"last_id":  "",
	}
}

func cloneModel(model map[string]any) map[string]any {
	cloned := make(map[string]any, len(model))
	maps.Copy(cloned, model)
	return cloned
}

func stringValue(model map[string]any, key string) string {
	value, _ := model[key].(string)
	return value
}

func encodeModelID(id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(id))
}

func splitThinkingSuffix(model string) (base, suffix string, hasSuffix bool) {
	lastOpen := strings.LastIndex(model, "(")
	if lastOpen == -1 || !strings.HasSuffix(model, ")") {
		return model, "", false
	}
	return model[:lastOpen], model[lastOpen+1 : len(model)-1], true
}
