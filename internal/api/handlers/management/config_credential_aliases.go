package management

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

func providerKeyAliasReference(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	mac := hmac.New(sha256.New, configYAMLPlaceholderKey[:])
	_, _ = mac.Write([]byte("provider-key-alias\x00" + key))
	return "alias_" + hex.EncodeToString(mac.Sum(nil)[:16])
}

// Use the persisted representation so a transactional config clone keeps the same reference.
func providerEntryReference(resource string, entry any) string {
	data, err := yaml.Marshal(entry)
	if err != nil {
		log.WithField("resource", resource).Error("failed to encode credential entry reference")
		return ""
	}
	mac := hmac.New(sha256.New, configYAMLPlaceholderKey[:])
	_, _ = mac.Write([]byte("provider-entry\x00" + resource + "\x00"))
	_, _ = mac.Write(data)
	return "entry_" + hex.EncodeToString(mac.Sum(nil)[:16])
}

func addProviderEntryReferences[T any](response map[string]any, cfg *config.Config, resource structListResource[T]) {
	rows, _ := response[resource.configKey].([]any)
	entries := resource.getList(cfg)
	for i := 0; i < min(len(rows), len(entries)); i++ {
		if row, ok := rows[i].(map[string]any); ok {
			row["entry-ref"] = providerEntryReference(resource.configKey, &entries[i])
		}
	}
}

func addProviderKeyAliasReferences(response map[string]any, cfg *config.Config) {
	addProviderEntryReferences(response, cfg, geminiKeyResource)
	addProviderEntryReferences(response, cfg, interactionsKeyResource)
	addProviderEntryReferences(response, cfg, claudeKeyResource)
	addProviderEntryReferences(response, cfg, codexKeyResource)
	addProviderEntryReferences(response, cfg, metaKeyResource)
	addProviderEntryReferences(response, cfg, xaiKeyResource)
	addProviderEntryReferences(response, cfg, vertexCompatKeyResource)
	setReference := func(row any, key string) {
		if item, ok := row.(map[string]any); ok {
			if reference := providerKeyAliasReference(key); reference != "" {
				item["alias-ref"] = reference
			}
		}
	}
	for section, keys := range cfg.ConfiguredProviderKeyRows() {
		rows, _ := response[section].([]any)
		for i, row := range rows {
			if section != "openai-compatibility" {
				setReference(row, keys[i][0])
				continue
			}
			provider, _ := row.(map[string]any)
			entries, _ := provider["api-key-entries"].([]any)
			for j, entry := range entries {
				setReference(entry, keys[i][j])
			}
		}
	}
}

func resolveProviderKeyAliasReference(cfg *config.Config, reference string) string {
	for _, rows := range cfg.ConfiguredProviderKeyRows() {
		for _, row := range rows {
			for _, key := range row {
				if providerKeyAliasReference(key) == reference {
					return strings.TrimSpace(key)
				}
			}
		}
	}
	return ""
}
