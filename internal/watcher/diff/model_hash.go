package diff

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
)

// ComputeOpenAICompatModelsHash returns a stable hash for OpenAI-compat models.
// Used to detect model list changes during hot reload.
func ComputeOpenAICompatModelsHash(models []config.OpenAICompatibilityModel) string {
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
						"|max-context-length=" + strconv.Itoa(model.MaxContextLength) +
						"|image=" + strconv.FormatBool(model.Image) +
						"|input-modalities=" + strings.Join(
						normalizeModelModalitiesForHash(model.InputModalities), ",",
					) +
						"|output-modalities=" + strings.Join(
						normalizeModelModalitiesForHash(model.OutputModalities), ",",
					) +
						"|is-compat=" + strconv.FormatBool(model.IsCompat) +
						thinkingHashSuffix(model.Thinking),
				)
			}
		},
	)
	return hashJoined(keys)
}

// ComputeVertexCompatModelsHash returns a stable hash for Vertex-compatible models.
func ComputeVertexCompatModelsHash(models []config.VertexCompatModel) string {
	return computeConfiguredModelsHash(
		models,
		func(model config.VertexCompatModel) (string, string, string, int, *registry.ThinkingSupport) {
			return model.Name, model.Alias, model.DisplayName, 0, model.Thinking
		},
	)
}

// ComputeClaudeModelsHash returns a stable hash for Claude model aliases.
func ComputeClaudeModelsHash(models []config.ClaudeModel) string {
	return computeConfiguredModelsHash(
		models,
		func(model config.ClaudeModel) (string, string, string, int, *registry.ThinkingSupport) {
			return model.Name, model.Alias, model.DisplayName, model.MaxContextLength, model.Thinking
		},
	)
}

// ComputeCodexModelsHash returns a stable hash for Codex-shaped model aliases.
func ComputeCodexModelsHash(models []config.CodexModel) string {
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
						"|max-context-length=" + strconv.Itoa(model.MaxContextLength) +
						"|force-mapping=" + strconv.FormatBool(model.ForceMapping) +
						"|is-compat=" + strconv.FormatBool(model.IsCompat) +
						thinkingHashSuffix(model.Thinking),
				)
			}
		},
	)
	return hashJoined(keys)
}

// ComputeGeminiModelsHash returns a stable hash for Gemini model aliases.
func ComputeGeminiModelsHash(models []config.GeminiModel) string {
	return computeConfiguredModelsHash(
		models,
		func(model config.GeminiModel) (string, string, string, int, *registry.ThinkingSupport) {
			return model.Name, model.Alias, model.DisplayName, model.MaxContextLength, model.Thinking
		},
	)
}

func computeConfiguredModelsHash[T any](
	models []T,
	metadata func(T) (name, alias, displayName string, maxContextLength int, thinking *registry.ThinkingSupport),
) string {
	keys := normalizeModelPairs(
		func(out func(key string)) {
			for _, model := range models {
				name, alias, displayName, maxContextLength, thinking := metadata(model)
				name = strings.TrimSpace(name)
				alias = strings.TrimSpace(alias)
				if name == "" && alias == "" {
					continue
				}
				out(
					strings.ToLower(name) + "|" + strings.ToLower(alias) +
						"|display-name=" + strings.TrimSpace(displayName) +
						"|max-context-length=" + strconv.Itoa(maxContextLength) +
						configuredModelCompatHashSuffix(model) +
						thinkingHashSuffix(thinking),
				)
			}
		},
	)
	return hashJoined(keys)
}

// ComputeExcludedModelsHash returns a normalized hash for excluded model lists.
func ComputeExcludedModelsHash(excluded []string) string {
	if len(excluded) == 0 {
		return ""
	}
	normalized := make([]string, 0, len(excluded))
	for _, entry := range excluded {
		if trimmed := strings.TrimSpace(entry); trimmed != "" {
			normalized = append(normalized, strings.ToLower(trimmed))
		}
	}
	if len(normalized) == 0 {
		return ""
	}
	sort.Strings(normalized)
	data, _ := json.Marshal(normalized)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func thinkingHashSuffix(support *registry.ThinkingSupport) string {
	data, _ := json.Marshal(support)
	return "|thinking=" + string(data)
}

func configuredModelCompatHashSuffix(model any) string {
	compatModel, ok := model.(interface{ GetIsCompat() bool })
	if !ok {
		return ""
	}
	return "|is-compat=" + strconv.FormatBool(compatModel.GetIsCompat())
}

func normalizeModelModalitiesForHash(raw []string) []string {
	if len(raw) == 0 {
		return nil
	}
	out := make([]string, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for _, item := range raw {
		modality := strings.ToLower(strings.TrimSpace(item))
		if modality == "" {
			continue
		}
		if _, exists := seen[modality]; exists {
			continue
		}
		seen[modality] = struct{}{}
		out = append(out, modality)
	}
	return out
}

func normalizeModelPairs(collect func(out func(key string))) []string {
	seen := make(map[string]struct{})
	keys := make([]string, 0)
	collect(
		func(key string) {
			if _, exists := seen[key]; exists {
				return
			}
			seen[key] = struct{}{}
			keys = append(keys, key)
		},
	)
	if len(keys) == 0 {
		return nil
	}
	sort.Strings(keys)
	return keys
}

func hashJoined(keys []string) string {
	if len(keys) == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.Join(keys, "\n")))
	return hex.EncodeToString(sum[:])
}
