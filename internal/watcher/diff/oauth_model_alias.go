package diff

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

// summarizeOAuthModelAlias summarizes OAuth model alias per channel.
func summarizeOAuthModelAlias(entries map[string][]config.OAuthModelAlias) map[string]modelsSummary {
	if len(entries) == 0 {
		return nil
	}
	out := make(map[string]modelsSummary, len(entries))
	for k, v := range entries {
		key := strings.ToLower(strings.TrimSpace(k))
		if key == "" {
			continue
		}
		out[key] = summarizeOAuthModelAliasList(v)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// oAuthModelAliasChanges compares OAuth model alias maps.
func oAuthModelAliasChanges(oldMap, newMap map[string][]config.OAuthModelAlias) ([]string, []string) {
	return diffSummaryMapChanges(
		summarizeOAuthModelAlias(oldMap),
		summarizeOAuthModelAlias(newMap),
		"oauth-model-alias",
	)
}

func summarizeOAuthModelAliasList(list []config.OAuthModelAlias) modelsSummary {
	if len(list) == 0 {
		return modelsSummary{}
	}
	seen := make(map[string]struct{}, len(list))
	normalized := make([]string, 0, len(list))
	for _, alias := range list {
		name := strings.ToLower(strings.TrimSpace(alias.Name))
		aliasVal := strings.ToLower(strings.TrimSpace(alias.Alias))
		if name == "" || aliasVal == "" {
			continue
		}
		key := name + "->" + aliasVal
		if alias.Fork {
			key += "|fork"
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		normalized = append(normalized, key)
	}
	if len(normalized) == 0 {
		return modelsSummary{}
	}
	sort.Strings(normalized)
	sum := sha256.Sum256([]byte(strings.Join(normalized, "|")))
	return modelsSummary{
		hash:  hex.EncodeToString(sum[:]),
		count: len(normalized),
	}
}
