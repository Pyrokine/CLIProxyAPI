package diff

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

// modelsSummary holds a hash and count for change detection of model/entry lists.
type modelsSummary struct {
	hash  string
	count int
}

// summarizeModelPairs computes a hash-based summary from model routing metadata.
func summarizeModelPairs[T any](models []T, accessor func(T) (name, alias, metadata string)) modelsSummary {
	if len(models) == 0 {
		return modelsSummary{}
	}
	keys := normalizeModelPairs(
		func(out func(key string)) {
			for _, model := range models {
				name, alias, metadata := accessor(model)
				name = strings.TrimSpace(name)
				alias = strings.TrimSpace(alias)
				if name == "" && alias == "" {
					continue
				}
				out(strings.ToLower(name) + "|" + strings.ToLower(alias) + metadata)
			}
		},
	)
	return modelsSummary{
		hash:  hashJoined(keys),
		count: len(keys),
	}
}

// summarizeGeminiModels hashes Gemini model aliases for change detection.
func summarizeGeminiModels(models []config.GeminiModel) modelsSummary {
	return summarizeModelPairs(
		models, func(m config.GeminiModel) (string, string, string) {
			return m.Name, m.Alias, configuredModelCompatHashSuffix(m) + thinkingHashSuffix(m.Thinking)
		},
	)
}

// summarizeClaudeModels hashes Claude model aliases for change detection.
func summarizeClaudeModels(models []config.ClaudeModel) modelsSummary {
	return summarizeModelPairs(
		models, func(m config.ClaudeModel) (string, string, string) {
			return m.Name, m.Alias, configuredModelCompatHashSuffix(m) + thinkingHashSuffix(m.Thinking)
		},
	)
}

// summarizeCodexModels hashes Codex-shaped model aliases for change detection.
func summarizeCodexModels(models []config.CodexModel) modelsSummary {
	if len(models) == 0 {
		return modelsSummary{}
	}
	keys := normalizeModelPairs(
		func(out func(key string)) {
			for _, model := range models {
				name := strings.TrimSpace(model.Name)
				alias := strings.TrimSpace(model.Alias)
				if name == "" && alias == "" {
					continue
				}
				out(
					strings.ToLower(name) + "|" + strings.ToLower(alias) +
						"|display-name=" + strings.TrimSpace(model.DisplayName) +
						"|max-context-length=" + fmt.Sprintf("%d", model.MaxContextLength) +
						"|force-mapping=" + fmt.Sprintf("%t", model.ForceMapping) +
						configuredModelCompatHashSuffix(model) +
						thinkingHashSuffix(model.Thinking),
				)
			}
		},
	)
	return modelsSummary{hash: hashJoined(keys), count: len(keys)}
}

// summarizeVertexModels hashes Vertex-compatible model aliases for change detection.
func summarizeVertexModels(models []config.VertexCompatModel) modelsSummary {
	if len(models) == 0 {
		return modelsSummary{}
	}
	names := make([]string, 0, len(models))
	for _, model := range models {
		name := strings.TrimSpace(model.Name)
		alias := strings.TrimSpace(model.Alias)
		if name == "" && alias == "" {
			continue
		}
		if alias != "" {
			name = alias
		}
		names = append(names, name+thinkingHashSuffix(model.Thinking))
	}
	if len(names) == 0 {
		return modelsSummary{}
	}
	sort.Strings(names)
	sum := sha256.Sum256([]byte(strings.Join(names, "|")))
	return modelsSummary{
		hash:  hex.EncodeToString(sum[:]),
		count: len(names),
	}
}

// diffSummaryMapChanges compares two summary maps and reports additions, removals, and updates.
func diffSummaryMapChanges(oldSummary, newSummary map[string]modelsSummary, label string) ([]string, []string) {
	keys := make(map[string]struct{}, len(oldSummary)+len(newSummary))
	for k := range oldSummary {
		keys[k] = struct{}{}
	}
	for k := range newSummary {
		keys[k] = struct{}{}
	}
	changes := make([]string, 0, len(keys))
	affected := make([]string, 0, len(keys))
	for key := range keys {
		oldInfo, okOld := oldSummary[key]
		newInfo, okNew := newSummary[key]
		switch {
		case okOld && !okNew:
			changes = append(changes, fmt.Sprintf("%s[%s]: removed", label, key))
			affected = append(affected, key)
		case !okOld && okNew:
			changes = append(changes, fmt.Sprintf("%s[%s]: added (%d entries)", label, key, newInfo.count))
			affected = append(affected, key)
		// noinspection GoDfaConstantCondition — both okOld and okNew are true here; hash comparison is meaningful.
		case oldInfo.hash != newInfo.hash:
			changes = append(
				changes,
				fmt.Sprintf("%s[%s]: updated (%d -> %d entries)", label, key, oldInfo.count, newInfo.count),
			)
			affected = append(affected, key)
		}
	}
	sort.Strings(changes)
	sort.Strings(affected)
	return changes, affected
}
