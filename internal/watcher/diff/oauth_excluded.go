package diff

import (
	"sort"
	"strings"
)

// summarizeExcludedModels normalizes and hashes an excluded-model list.
func summarizeExcludedModels(list []string) modelsSummary {
	if len(list) == 0 {
		return modelsSummary{}
	}
	seen := make(map[string]struct{}, len(list))
	normalized := make([]string, 0, len(list))
	for _, entry := range list {
		if trimmed := strings.ToLower(strings.TrimSpace(entry)); trimmed != "" {
			if _, exists := seen[trimmed]; exists {
				continue
			}
			seen[trimmed] = struct{}{}
			normalized = append(normalized, trimmed)
		}
	}
	sort.Strings(normalized)
	return modelsSummary{
		hash:  ComputeExcludedModelsHash(normalized),
		count: len(normalized),
	}
}

// summarizeOAuthExcludedModels summarizes OAuth excluded models per provider.
func summarizeOAuthExcludedModels(entries map[string][]string) map[string]modelsSummary {
	if len(entries) == 0 {
		return nil
	}
	out := make(map[string]modelsSummary, len(entries))
	for k, v := range entries {
		key := strings.ToLower(strings.TrimSpace(k))
		if key == "" {
			continue
		}
		out[key] = summarizeExcludedModels(v)
	}
	return out
}

// OAuthExcludedModelChanges compares OAuth excluded models maps.
func OAuthExcludedModelChanges(oldMap, newMap map[string][]string) ([]string, []string) {
	return diffSummaryMapChanges(
		summarizeOAuthExcludedModels(oldMap),
		summarizeOAuthExcludedModels(newMap),
		"oauth-excluded-models",
	)
}
