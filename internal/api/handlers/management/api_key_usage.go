package management

import (
	"net/http"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
)

type apiKeyUsageEntry struct {
	Success        int64                          `json:"success"`
	Failed         int64                          `json:"failed"`
	RecentRequests []coreauth.RecentRequestBucket `json:"recent_requests"`
}

func mergeRecentRequestBuckets(dst, src []coreauth.RecentRequestBucket) []coreauth.RecentRequestBucket {
	if len(dst) == 0 {
		return src
	}
	if len(src) == 0 {
		return dst
	}
	if len(dst) != len(src) {
		n := min(len(src), len(dst))
		for i := range n {
			dst[i].Success += src[i].Success
			dst[i].Failed += src[i].Failed
		}
		return dst
	}
	for i := range dst {
		dst[i].Success += src[i].Success
		dst[i].Failed += src[i].Failed
	}
	return dst
}

func (h *Handler) GetAPIKeyUsage(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return
	}

	h.mu.Lock()
	manager := h.authManager
	h.mu.Unlock()
	if manager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}

	type credentialKey struct{ baseURL, apiKey string }
	type usageGroup struct {
		index string
		entry apiKeyUsageEntry
	}
	now := time.Now()
	groups := make(map[string]map[credentialKey]*usageGroup)
	for _, auth := range manager.List() {
		if auth == nil {
			continue
		}
		kind, apiKey := auth.AccountInfo()
		if !strings.EqualFold(strings.TrimSpace(kind), "api_key") {
			continue
		}
		apiKey = strings.TrimSpace(apiKey)
		if apiKey == "" {
			continue
		}
		baseURL := ""
		if auth.Attributes != nil {
			baseURL = strings.TrimSpace(auth.Attributes["base_url"])
			if baseURL == "" {
				baseURL = strings.TrimSpace(auth.Attributes["base-url"])
			}
		}
		provider := strings.ToLower(strings.TrimSpace(auth.Provider))
		if compatName := strings.TrimSpace(auth.Attributes["compat_name"]); compatName != "" {
			provider = strings.ToLower(compatName)
		}
		if provider == "" {
			provider = "unknown"
		}

		recent := auth.RecentRequestsSnapshot(now)
		providerGroups := groups[provider]
		if providerGroups == nil {
			providerGroups = make(map[credentialKey]*usageGroup)
			groups[provider] = providerGroups
		}
		key := credentialKey{baseURL: baseURL, apiKey: apiKey}
		if existing := providerGroups[key]; existing != nil {
			if index := auth.EnsureIndex(); index < existing.index {
				existing.index = index
			}
			existing.entry.Success += auth.Success
			existing.entry.Failed += auth.Failed
			existing.entry.RecentRequests = mergeRecentRequestBuckets(existing.entry.RecentRequests, recent)
			continue
		}
		providerGroups[key] = &usageGroup{
			index: auth.EnsureIndex(),
			entry: apiKeyUsageEntry{
				Success: auth.Success, Failed: auth.Failed, RecentRequests: recent,
			},
		}
	}

	out := make(map[string]map[string]apiKeyUsageEntry, len(groups))
	for provider, providerGroups := range groups {
		maskedCounts := make(map[string]int, len(providerGroups))
		for key := range providerGroups {
			maskedCounts[key.baseURL+"|"+util.HideAPIKey(key.apiKey)]++
		}
		entries := make(map[string]apiKeyUsageEntry, len(providerGroups))
		for key, group := range providerGroups {
			maskedKey := key.baseURL + "|" + util.HideAPIKey(key.apiKey)
			if maskedCounts[maskedKey] > 1 {
				maskedKey += "|" + group.index
			}
			entries[maskedKey] = group.entry
		}
		out[provider] = entries
	}
	c.JSON(http.StatusOK, out)
}
