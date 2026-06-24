package modelcatalog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

var storeMu sync.RWMutex

func Path(configPath string) string {
	if strings.TrimSpace(configPath) == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(configPath), FileName)
}

func Load(configPath string) (File, error) {
	filePath := Path(configPath)
	if filePath == "" {
		return File{}, fmt.Errorf("config file path not set")
	}
	storeMu.RLock()
	defer storeMu.RUnlock()
	return readFile(filePath)
}

func LoadOrInit(configPath string, cfg *config.Config) (File, error) {
	filePath := Path(configPath)
	if filePath == "" {
		return File{}, fmt.Errorf("config file path not set")
	}
	storeMu.Lock()
	defer storeMu.Unlock()
	if _, err := os.Stat(filePath); err == nil {
		doc, err := readFileUnlocked(filePath)
		if err != nil {
			return File{}, err
		}
		if cfg != nil && ApplyOAuthModelAlias(&doc, cfg.OAuthModelAlias) {
			if err := writeFileUnlocked(filePath, doc); err != nil {
				return File{}, err
			}
		}
		return doc, nil
	} else if !os.IsNotExist(err) {
		return File{}, err
	}
	doc := DefaultFile()
	if err := applyLegacyPrices(filepath.Join(filepath.Dir(filePath), "model-prices.json"), &doc); err != nil {
		return File{}, err
	}
	applyLegacyConfig(cfg, &doc)
	doc.Sanitize()
	if err := writeFileUnlocked(filePath, doc); err != nil {
		return File{}, err
	}
	return doc, nil
}

func Save(configPath string, doc File) error {
	filePath := Path(configPath)
	if filePath == "" {
		return fmt.Errorf("config file path not set")
	}
	storeMu.Lock()
	defer storeMu.Unlock()
	doc.Sanitize()
	return writeFileUnlocked(filePath, doc)
}

func ReplacePrices(configPath string, prices map[string]Price) (File, error) {
	normalizedPrices := make(map[string]Price, len(prices))
	for name, price := range prices {
		name = normalizeModelName(name)
		if name != "" {
			normalizedPrices[name] = price
		}
	}
	doc, err := LoadOrInit(configPath, nil)
	if err != nil {
		return File{}, err
	}
	for i := range doc.Models {
		price, ok := normalizedPrices[doc.Models[i].Name]
		if !ok {
			doc.Models[i].Price = nil
			continue
		}
		doc.Models[i].Price = &price
	}
	names := make([]string, 0, len(normalizedPrices))
	for name := range normalizedPrices {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, ok := doc.Find(name); ok {
			continue
		}
		model := doc.EnsureModel(name, true)
		price := normalizedPrices[name]
		model.Price = &price
	}
	doc.Sanitize()
	if err := Save(configPath, doc); err != nil {
		return File{}, err
	}
	return doc, nil
}

func Prices(configPath string) (map[string]Price, error) {
	doc, err := Load(configPath)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Price)
	for _, model := range doc.Models {
		if model.Price == nil || normalizeModelName(model.Name) == "" {
			continue
		}
		out[model.Name] = *model.Price
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func readFile(filePath string) (File, error) {
	return readFileUnlocked(filePath)
}

func readFileUnlocked(filePath string) (File, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return File{}, err
	}
	var doc File
	if err := json.Unmarshal(data, &doc); err != nil {
		return File{}, err
	}
	doc.Sanitize()
	return doc, nil
}

func writeFileUnlocked(filePath string, doc File) error {
	if err := os.MkdirAll(filepath.Dir(filePath), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmpPath := filePath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpPath, filePath)
}

func applyLegacyPrices(filePath string, doc *File) error {
	if doc == nil || strings.TrimSpace(filePath) == "" {
		return nil
	}
	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read legacy model-prices.json: %w", err)
	}
	var prices map[string]Price
	if err := json.Unmarshal(data, &prices); err != nil {
		return fmt.Errorf("parse legacy model-prices.json: %w", err)
	}
	normalizedPrices := make(map[string]Price, len(prices))
	for name, price := range prices {
		name = normalizeModelName(name)
		if name != "" {
			normalizedPrices[name] = price
		}
	}
	names := make([]string, 0, len(normalizedPrices))
	for name := range normalizedPrices {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		model := doc.EnsureModel(name, true)
		price := normalizedPrices[name]
		model.Price = &price
	}
	return nil
}

func ReplaceOAuthModelAlias(doc *File, aliases map[string][]config.OAuthModelAlias) {
	if doc == nil {
		return
	}
	for i := range doc.Models {
		doc.Models[i].Aliases = nil
	}
	ApplyOAuthModelAlias(doc, aliases)
	doc.Sanitize()
}

func ReplaceOAuthModelAliasChannel(doc *File, channel string, aliases []config.OAuthModelAlias) {
	if doc == nil {
		return
	}
	deleteOAuthModelAliasChannel(doc, channel)
	ApplyOAuthModelAlias(doc, map[string][]config.OAuthModelAlias{channel: aliases})
	doc.Sanitize()
}

func DeleteOAuthModelAliasChannel(doc *File, channel string) bool {
	if doc == nil {
		return false
	}
	changed := deleteOAuthModelAliasChannel(doc, channel)
	if changed {
		doc.Sanitize()
	}
	return changed
}

func deleteOAuthModelAliasChannel(doc *File, channel string) bool {
	channel = strings.ToLower(strings.TrimSpace(channel))
	if doc == nil || channel == "" {
		return false
	}
	changed := false
	for i := range doc.Models {
		if !modelMatchesOAuthAliasChannel(doc.Models[i], channel) || len(doc.Models[i].Aliases) == 0 {
			continue
		}
		doc.Models[i].Aliases = nil
		changed = true
	}
	return changed
}

func modelMatchesOAuthAliasChannel(model Model, channel string) bool {
	channel = strings.ToLower(strings.TrimSpace(channel))
	if channel == "" {
		return false
	}
	modelChannel := strings.ToLower(strings.TrimSpace(model.Channel))
	if modelChannel == channel {
		return true
	}
	for _, aliasChannel := range oauthAliasChannelsForModel(model.Provider, modelChannel) {
		if aliasChannel == channel {
			return true
		}
	}
	return false
}

func ApplyOAuthModelAlias(doc *File, aliases map[string][]config.OAuthModelAlias) bool {
	if doc == nil || len(aliases) == 0 {
		return false
	}
	changed := false
	channels := make([]string, 0, len(aliases))
	for channel := range aliases {
		channels = append(channels, channel)
	}
	sort.Strings(channels)
	for _, channel := range channels {
		for _, alias := range aliases[channel] {
			if applyOAuthModelAliasEntry(doc, channel, alias) {
				changed = true
			}
		}
	}
	if changed {
		doc.Sanitize()
	}
	return changed
}

func applyOAuthModelAliasEntry(doc *File, channel string, alias config.OAuthModelAlias) bool {
	if doc == nil {
		return false
	}
	name := normalizeModelName(alias.Name)
	aliasName := normalizeModelName(alias.Alias)
	if name == "" || aliasName == "" || strings.EqualFold(name, aliasName) {
		return false
	}
	model := doc.EnsureModel(name, true)
	if model == nil {
		return false
	}
	if provider := inferProvider(channel, name); model.Provider == "" || model.Provider == "Other" {
		model.Provider = provider
	}
	if inferredChannel := inferChannel(channel, name); model.Channel == "" || model.Channel == "other" {
		model.Channel = inferredChannel
	}
	for _, existing := range model.Aliases {
		if strings.EqualFold(existing, aliasName) {
			return false
		}
	}
	model.Aliases = append(model.Aliases, aliasName)
	return true
}

func applyLegacyConfig(cfg *config.Config, doc *File) {
	if cfg == nil || doc == nil {
		return
	}
	ApplyOAuthModelAlias(doc, cfg.OAuthModelAlias)
	for _, entry := range cfg.ModelCatalog.Models {
		name := normalizeModelName(entry.Name)
		if name == "" {
			continue
		}
		model := doc.EnsureModel(name, true)
		if provider := strings.TrimSpace(entry.Provider); provider != "" {
			model.Provider = provider
		}
		if channel := strings.TrimSpace(entry.Channel); channel != "" {
			model.Channel = strings.ToLower(channel)
		}
		if group := strings.TrimSpace(entry.Group); group != "" {
			model.Group = group
		}
		if displayName := strings.TrimSpace(entry.DisplayName); displayName != "" {
			model.DisplayName = displayName
		}
		if entry.Enabled != nil {
			model.Enabled = *entry.Enabled
		}
	}
}
