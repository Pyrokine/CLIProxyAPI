package modelcatalog

import (
	"sort"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

const (
	FileName        = "models.json"
	SchemaVersion   = 1
	DefaultsVersion = "2026-06-23"
)

type Price struct {
	Prompt     float64 `json:"prompt"`
	Completion float64 `json:"completion"`
	Cache      float64 `json:"cache"`
}

type Model struct {
	Name               string   `json:"name"`
	Provider           string   `json:"provider"`
	Channel            string   `json:"channel"`
	Group              string   `json:"group"`
	DisplayName        string   `json:"display_name,omitempty"`
	UserCreated        bool     `json:"user_created"`
	Enabled            bool     `json:"enabled"`
	Aliases            []string `json:"aliases,omitempty"`
	ForceMappedAliases []string `json:"force_mapped_aliases,omitempty"`
	Price              *Price   `json:"price,omitempty"`
}

type File struct {
	SchemaVersion        int      `json:"schema_version"`
	DefaultsVersion      string   `json:"defaults_version"`
	RemovedDefaultModels []string `json:"removed_default_models,omitempty"`
	Models               []Model  `json:"models"`
}

func (f *File) Sanitize() {
	if f == nil {
		return
	}
	if f.SchemaVersion == 0 {
		f.SchemaVersion = SchemaVersion
	}
	if strings.TrimSpace(f.DefaultsVersion) == "" {
		f.DefaultsVersion = DefaultsVersion
	}
	removed := uniqueStrings(f.RemovedDefaultModels)
	models := make([]Model, 0, len(f.Models))
	seen := make(map[string]struct{}, len(f.Models))
	for _, model := range f.Models {
		model.sanitize()
		if model.Name == "" {
			continue
		}
		key := modelKey(model.Name)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		models = append(models, model)
	}
	sort.Slice(
		models, func(i, j int) bool {
			if models[i].Provider == models[j].Provider {
				if models[i].Group == models[j].Group {
					return strings.ToLower(models[i].Name) < strings.ToLower(models[j].Name)
				}
				return models[i].Group < models[j].Group
			}
			return providerOrder(models[i].Provider) < providerOrder(models[j].Provider)
		},
	)
	f.RemovedDefaultModels = removed
	f.Models = models
}

func (m *Model) sanitize() {
	if m == nil {
		return
	}
	m.Name = normalizeModelName(m.Name)
	m.Provider = strings.TrimSpace(m.Provider)
	m.Channel = strings.ToLower(strings.TrimSpace(m.Channel))
	if m.Provider == "" {
		m.Provider = inferProvider(m.Channel, m.Name)
	}
	m.Group = strings.TrimSpace(m.Group)
	if m.Group == "" {
		m.Group = inferGroup(m.Provider, m.Name)
	}
	m.DisplayName = strings.TrimSpace(m.DisplayName)
	m.Aliases = uniqueStrings(m.Aliases)
	m.ForceMappedAliases = filterStringsBySet(uniqueStrings(m.ForceMappedAliases), m.Aliases)
}

func (f *File) Find(name string) (*Model, bool) {
	if f == nil {
		return nil, false
	}
	key := modelKey(name)
	if key == "" {
		return nil, false
	}
	for i := range f.Models {
		if modelKey(f.Models[i].Name) == key {
			return &f.Models[i], true
		}
	}
	return nil, false
}

func (f *File) EnsureModel(name string, userCreated bool) *Model {
	name = normalizeModelName(name)
	if name == "" {
		return nil
	}
	if existing, ok := f.Find(name); ok {
		return existing
	}
	model := Model{
		Name:        name,
		Provider:    inferProvider("", name),
		Channel:     inferChannel("", name),
		Group:       inferGroup(inferProvider("", name), name),
		DisplayName: displayNameFromName(name),
		UserCreated: userCreated,
		Enabled:     true,
	}
	f.Models = append(f.Models, model)
	return &f.Models[len(f.Models)-1]
}

func (f *File) RemoveModel(name string, defaults File) bool {
	if f == nil {
		return false
	}
	key := modelKey(name)
	if key == "" {
		return false
	}
	removed := false
	models := f.Models[:0]
	for _, model := range f.Models {
		if modelKey(model.Name) == key {
			removed = true
			if !model.UserCreated {
				f.RemovedDefaultModels = append(f.RemovedDefaultModels, model.Name)
			}
			continue
		}
		models = append(models, model)
	}
	f.Models = models
	if _, ok := defaults.Find(name); ok {
		f.RemovedDefaultModels = append(f.RemovedDefaultModels, normalizeModelName(name))
	}
	f.RemovedDefaultModels = uniqueStrings(f.RemovedDefaultModels)
	return removed
}

func (f *File) ToModelCatalogModels() []config.ModelCatalogModel {
	if f == nil {
		return nil
	}
	out := make([]config.ModelCatalogModel, 0, len(f.Models))
	for _, model := range f.Models {
		name := normalizeModelName(model.Name)
		if name == "" {
			continue
		}
		enabled := model.Enabled
		out = append(
			out, config.ModelCatalogModel{
				Name:        name,
				Provider:    strings.ToLower(strings.TrimSpace(model.Provider)),
				Channel:     strings.ToLower(strings.TrimSpace(model.Channel)),
				Group:       strings.TrimSpace(model.Group),
				DisplayName: strings.TrimSpace(model.DisplayName),
				Aliases:     append([]string(nil), model.Aliases...),
				Enabled:     &enabled,
			},
		)
	}
	return out
}

func (f *File) ToOAuthModelAlias() map[string][]config.OAuthModelAlias {
	if f == nil {
		return nil
	}
	out := make(map[string][]config.OAuthModelAlias)
	for _, model := range f.Models {
		channel := strings.ToLower(strings.TrimSpace(model.Channel))
		name := normalizeModelName(model.Name)
		if channel == "" || name == "" || len(model.Aliases) == 0 {
			continue
		}
		forceMappedAliases := stringSet(model.ForceMappedAliases)
		for _, alias := range model.Aliases {
			alias = strings.TrimSpace(alias)
			if alias == "" || strings.EqualFold(alias, name) {
				continue
			}
			forceMapping := false
			if _, ok := forceMappedAliases[strings.ToLower(alias)]; ok {
				forceMapping = true
			}
			for _, aliasChannel := range oauthAliasChannelsForModel(model.Provider, channel) {
				out[aliasChannel] = append(
					out[aliasChannel], config.OAuthModelAlias{
						Name: name, Alias: alias, Fork: true, ForceMapping: forceMapping,
					},
				)
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	cfg := config.Config{OAuthModelAlias: out}
	cfg.SanitizeOAuthModelAlias()
	return cfg.OAuthModelAlias
}

func oauthAliasChannelsForModel(provider, channel string) []string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	channel = strings.ToLower(strings.TrimSpace(channel))
	switch channel {
	case "gemini":
		return []string{"gemini-cli", "vertex", "aistudio", "antigravity"}
	case "gemini-cli", "vertex", "aistudio", "antigravity", "claude", "codex", "qwen", "iflow", "kimi":
		return []string{channel}
	}
	if provider == "gemini" {
		return []string{"gemini-cli", "vertex", "aistudio", "antigravity"}
	}
	return nil
}

func normalizeModelName(name string) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(name), "models/"))
}

func modelKey(name string) string {
	return strings.ToLower(normalizeModelName(name))
}

func uniqueStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func filterStringsBySet(values, allowed []string) []string {
	if len(values) == 0 || len(allowed) == 0 {
		return nil
	}
	allowedSet := stringSet(allowed)
	out := values[:0]
	for _, value := range values {
		if _, ok := allowedSet[strings.ToLower(strings.TrimSpace(value))]; ok {
			out = append(out, value)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func stringSet(values []string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		key := strings.ToLower(strings.TrimSpace(value))
		if key != "" {
			out[key] = struct{}{}
		}
	}
	return out
}
