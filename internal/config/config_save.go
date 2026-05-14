package config

import (
	"bytes"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// SaveConfigPreserveComments writes the config back to YAML while preserving existing comments
// and key ordering by loading the original file into a yaml.Node tree and updating values in-place.
func setConfigAPIKeyProviderEntries(root *yaml.Node, keys []string) {
	if root == nil || root.Kind != yaml.MappingNode {
		return
	}
	if len(keys) == 0 {
		return
	}
	authNode := getOrCreateMapValue(root, "auth")
	providersNode := getOrCreateMapValue(authNode, "providers")
	configAPIKeyNode := getOrCreateMapValue(providersNode, "config-api-key")
	entriesNode := getOrCreateMapValue(configAPIKeyNode, "api-key-entries")
	entriesNode.Kind = yaml.SequenceNode
	entriesNode.Tag = "!!seq"
	entriesNode.Style = 0
	entriesNode.Content = entriesNode.Content[:0]
	for _, key := range normalizeStringSlice(keys) {
		entry := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		entry.Content = []*yaml.Node{
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: "api-key"},
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		}
		entriesNode.Content = append(entriesNode.Content, entry)
	}
	if idx := findMapKeyIndex(configAPIKeyNode, "api-keys"); idx >= 0 {
		removeMapKey(configAPIKeyNode, "api-keys")
	}
}

func SaveConfigPreserveComments(configFile string, cfg *Config) error {
	persistCfg := cfg
	// Load original YAML as a node tree to preserve comments and ordering.
	data, err := os.ReadFile(configFile)
	if err != nil {
		return err
	}

	var original yaml.Node
	if err = yaml.Unmarshal(data, &original); err != nil {
		return err
	}
	if original.Kind != yaml.DocumentNode || len(original.Content) == 0 {
		return fmt.Errorf("invalid yaml document structure")
	}
	if original.Content[0] == nil || original.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("expected root mapping node")
	}

	// Marshal the current cfg to YAML, then unmarshal to a yaml.Node we can merge from.
	rendered, err := yaml.Marshal(persistCfg)
	if err != nil {
		return err
	}
	var generated yaml.Node
	if err = yaml.Unmarshal(rendered, &generated); err != nil {
		return err
	}
	if generated.Kind != yaml.DocumentNode || len(generated.Content) == 0 || generated.Content[0] == nil {
		return fmt.Errorf("invalid generated yaml structure")
	}
	if generated.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("expected generated root mapping node")
	}

	// Remove deprecated sections before merging back the sanitized config.
	removeLegacyAuthBlock(original.Content[0])
	removeLegacyOpenAICompatAPIKeys(original.Content[0])
	removeLegacyAmpKeys(original.Content[0])
	removeLegacyGenerativeLanguageKeys(original.Content[0])

	pruneMappingToGeneratedKeys(original.Content[0], generated.Content[0], "oauth-excluded-models")
	pruneMappingToGeneratedKeys(original.Content[0], generated.Content[0], "oauth-model-alias")
	setConfigAPIKeyProviderEntries(original.Content[0], persistCfg.APIKeys)

	// Merge generated into original in-place, preserving comments/order of existing nodes.
	mergeMappingPreserve(original.Content[0], generated.Content[0])
	normalizeCollectionNodeStyles(original.Content[0])

	// Write back.
	return writeYAMLNode(configFile, &original)
}

// SaveConfigPreserveCommentsUpdateNestedScalar updates a nested scalar key path like ["a","b"]
// while preserving comments and positions.
func SaveConfigPreserveCommentsUpdateNestedScalar(configFile string, path []string, value string) error {
	data, err := os.ReadFile(configFile)
	if err != nil {
		return err
	}
	var root yaml.Node
	if err = yaml.Unmarshal(data, &root); err != nil {
		return err
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return fmt.Errorf("invalid yaml document structure")
	}
	node := root.Content[0]
	// descend mapping nodes following path
	for i, key := range path {
		if i == len(path)-1 {
			// set final scalar
			v := getOrCreateMapValue(node, key)
			v.Kind = yaml.ScalarNode
			v.Tag = "!!str"
			v.Value = value
		} else {
			next := getOrCreateMapValue(node, key)
			if next.Kind != yaml.MappingNode {
				next.Kind = yaml.MappingNode
				next.Tag = "!!map"
			}
			node = next
		}
	}
	return writeYAMLNode(configFile, &root)
}

// writeYAMLNode encodes a yaml.Node to the given file with normalized comment indentation.
func writeYAMLNode(filePath string, node *yaml.Node) error {
	f, err := os.OpenFile(filePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err = enc.Encode(node); err != nil {
		_ = enc.Close()
		return err
	}
	if err = enc.Close(); err != nil {
		return err
	}
	data := NormalizeCommentIndentation(buf.Bytes())
	_, err = f.Write(data)
	return err
}

// EnsureDefaultsInFile re-saves the config to the YAML file, ensuring all known fields
// (including those with default/zero values) are written out. This is called once at startup
// so the source file always contains every configurable field.
func EnsureDefaultsInFile(configFile string, cfg *Config) {
	if configFile == "" || cfg == nil {
		return
	}
	if err := SaveConfigPreserveComments(configFile, cfg); err != nil {
		// Non-fatal: log the error but don't prevent startup
		fmt.Fprintf(os.Stderr, "config: failed to ensure defaults in %s: %v\n", configFile, err)
	}
}
