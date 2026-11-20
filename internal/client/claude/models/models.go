// Package models builds model catalogs for Anthropic clients.
package models

import (
	"encoding/base64"
	"maps"
	"slices"
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
			resolved[providerID] = providerID
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
		// 旧格式与 x-/c- 重叠时，由目录中的实际模型消除歧义
		if legacy := ResolveClaudeModelIDPrefix(base); legacy != base && c.resolved[legacy] == legacy {
			resolved = legacy
		} else {
			resolved = resolveClientModelID(base)
		}
	}
	if resolved == base {
		return id
	}
	if hasSuffix {
		return resolved + "(" + suffix + ")"
	}
	return resolved
}

// buildResponse 返回使用旧版模型 ID 的有序列表
func buildResponse(availableModels []map[string]any, disableCloaking bool) map[string]any {
	if disableCloaking {
		return NewCatalog(availableModels, true).Response()
	}
	legacyModels := make([]map[string]any, len(availableModels))
	for index, model := range availableModels {
		cloned := cloneModel(model)
		if id, ok := cloned["id"].(string); ok {
			cloned["id"] = EnsureClaudeModelIDPrefix(id)
		}
		legacyModels[index] = cloned
	}
	return NewCatalog(legacyModels, true).Response()
}

// EnsureClaudeModelIDPrefix 将非 Claude 模型 ID 编码为旧版反转格式
func EnsureClaudeModelIDPrefix(id string) string {
	if id == "" || strings.HasPrefix(id, "claude-") {
		return id
	}
	return clientModelPrefix + reverseModelID(id)
}

// ResolveClaudeModelIDPrefix 还原旧版模型 ID，保留思考后缀
func ResolveClaudeModelIDPrefix(id string) string {
	base, suffix, hasSuffix := splitThinkingSuffix(id)
	if !strings.HasPrefix(base, clientModelPrefix) || base == clientModelPrefix {
		return id
	}
	resolved := reverseModelID(strings.TrimPrefix(base, clientModelPrefix))
	if hasSuffix {
		return resolved + "(" + suffix + ")"
	}
	return resolved
}

func reverseModelID(id string) string {
	characters := []rune(id)
	slices.Reverse(characters)
	return string(characters)
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
		return ResolveClaudeModelIDPrefix(id)
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
