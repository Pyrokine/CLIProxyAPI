package config

import (
	"strings"
	"sync/atomic"
)

// ModelCatalogConfig stores backend-owned model display and availability overrides.
type ModelCatalogConfig struct {
	Models []ModelCatalogModel `yaml:"models,omitempty" json:"models,omitempty"`
}

func (c ModelCatalogConfig) IsZero() bool {
	return len(c.Models) == 0
}

// ModelCatalogModel describes a model entry owned by the backend config.
type ModelCatalogModel struct {
	Name        string   `yaml:"name" json:"name"`
	Provider    string   `yaml:"provider,omitempty" json:"provider,omitempty"`
	Channel     string   `yaml:"channel,omitempty" json:"channel,omitempty"`
	Group       string   `yaml:"group,omitempty" json:"group,omitempty"`
	DisplayName string   `yaml:"display-name,omitempty" json:"display-name,omitempty"`
	Aliases     []string `yaml:"aliases,omitempty" json:"aliases,omitempty"`
	Enabled     *bool    `yaml:"enabled,omitempty" json:"enabled,omitempty"`
}

func (m ModelCatalogModel) IsEnabled() bool {
	return m.Enabled == nil || *m.Enabled
}

func (cfg *Config) SanitizeModelCatalog() {
	if cfg == nil {
		return
	}
	cfg.SDKConfig.SanitizeModelCatalog()
}

func (cfg *SDKConfig) SanitizeModelCatalog() {
	if cfg == nil || len(cfg.ModelCatalog.Models) == 0 {
		return
	}
	seen := make(map[string]struct{}, len(cfg.ModelCatalog.Models))
	out := make([]ModelCatalogModel, 0, len(cfg.ModelCatalog.Models))
	for _, entry := range cfg.ModelCatalog.Models {
		entry.Name = strings.TrimSpace(entry.Name)
		if entry.Name == "" {
			continue
		}
		key := strings.ToLower(entry.Name)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		entry.Provider = strings.ToLower(strings.TrimSpace(entry.Provider))
		entry.Channel = strings.ToLower(strings.TrimSpace(entry.Channel))
		entry.Group = strings.TrimSpace(entry.Group)
		entry.DisplayName = strings.TrimSpace(entry.DisplayName)
		entry.Aliases = sanitizeModelCatalogAliases(entry.Aliases, entry.Name)
		out = append(out, entry)
	}
	cfg.ModelCatalog.Models = out
}

func (cfg *SDKConfig) ModelCatalogEntry(model string) (ModelCatalogModel, bool) {
	if cfg == nil || len(cfg.ModelCatalog.Models) == 0 {
		return ModelCatalogModel{}, false
	}
	model = strings.TrimSpace(strings.TrimPrefix(model, "models/"))
	if model == "" {
		return ModelCatalogModel{}, false
	}
	for _, entry := range cfg.ModelCatalog.Models {
		for _, name := range modelCatalogMatchNames(entry) {
			if modelCatalogNamesMatch(name, model) {
				return entry, true
			}
		}
	}
	return ModelCatalogModel{}, false
}

type runtimeModelCatalogSnapshot struct {
	enabled map[string]bool
}

var runtimeModelCatalog atomic.Value

func SetRuntimeModelCatalog(models []ModelCatalogModel) {
	snapshot := runtimeModelCatalogSnapshot{enabled: make(map[string]bool, len(models))}
	for _, entry := range models {
		for _, name := range modelCatalogMatchNames(entry) {
			name = strings.TrimSpace(strings.TrimPrefix(name, "models/"))
			if name == "" {
				continue
			}
			snapshot.enabled[strings.ToLower(name)] = entry.IsEnabled()
		}
	}
	runtimeModelCatalog.Store(snapshot)
}

func (cfg *SDKConfig) ModelCatalogModelEnabled(model string) bool {
	model = strings.TrimSpace(strings.TrimPrefix(model, "models/"))
	if model == "" {
		return true
	}
	if raw := runtimeModelCatalog.Load(); raw != nil {
		if snapshot, ok := raw.(runtimeModelCatalogSnapshot); ok && snapshot.enabled != nil {
			for _, candidate := range modelCatalogNormalizedCandidates(model) {
				if enabled, exists := snapshot.enabled[strings.ToLower(candidate)]; exists {
					return enabled
				}
			}
		}
	}
	entry, ok := cfg.ModelCatalogEntry(model)
	return !ok || entry.IsEnabled()
}

func sanitizeModelCatalogAliases(aliases []string, name string) []string {
	if len(aliases) == 0 {
		return nil
	}
	name = strings.TrimSpace(strings.TrimPrefix(name, "models/"))
	seen := make(map[string]struct{}, len(aliases))
	out := make([]string, 0, len(aliases))
	for _, alias := range aliases {
		alias = strings.TrimSpace(strings.TrimPrefix(alias, "models/"))
		if alias == "" || strings.EqualFold(alias, name) {
			continue
		}
		key := strings.ToLower(alias)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, alias)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func modelCatalogMatchNames(entry ModelCatalogModel) []string {
	names := make([]string, 0, 1+len(entry.Aliases))
	names = append(names, entry.Name)
	names = append(names, entry.Aliases...)
	seen := make(map[string]struct{}, len(names))
	out := make([]string, 0, len(names))
	for _, name := range names {
		for _, candidate := range modelCatalogNormalizedCandidates(name) {
			key := strings.ToLower(candidate)
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, candidate)
		}
	}
	return out
}

func modelCatalogNamesMatch(left, right string) bool {
	leftCandidates := modelCatalogNormalizedCandidates(left)
	rightCandidates := modelCatalogNormalizedCandidates(right)
	for _, leftCandidate := range leftCandidates {
		for _, rightCandidate := range rightCandidates {
			if strings.EqualFold(leftCandidate, rightCandidate) {
				return true
			}
		}
	}
	return false
}

func modelCatalogNormalizedCandidates(model string) []string {
	candidates := modelMatchCandidates(model)
	seen := make(map[string]struct{}, len(candidates))
	out := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(strings.TrimPrefix(candidate, "models/"))
		if candidate == "" {
			continue
		}
		key := strings.ToLower(candidate)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, candidate)
	}
	return out
}
