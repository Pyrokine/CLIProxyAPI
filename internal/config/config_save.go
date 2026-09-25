package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"syscall"

	"gopkg.in/yaml.v3"
)

// SaveConfigPreserveComments writes the config back to YAML while preserving existing comments
// and key ordering by loading the original file into a yaml.Node tree and updating values in-place.
func setConfigAPIKeyProviderEntries(root *yaml.Node, keys []string) {
	if root == nil || root.Kind != yaml.MappingNode {
		return
	}
	if len(keys) == 0 {
		authIndex := findMapKeyIndex(root, "auth")
		if authIndex < 0 {
			return
		}
		authNode := root.Content[authIndex+1]
		providersIndex := findMapKeyIndex(authNode, "providers")
		if providersIndex < 0 {
			return
		}
		providersNode := authNode.Content[providersIndex+1]
		providerIndex := findMapKeyIndex(providersNode, "config-api-key")
		if providerIndex < 0 {
			return
		}
		configAPIKeyNode := providersNode.Content[providerIndex+1]
		removeMapKey(configAPIKeyNode, "api-key-entries")
		removeMapKey(configAPIKeyNode, "api-keys")
		if len(configAPIKeyNode.Content) == 0 {
			removeMapKey(providersNode, "config-api-key")
		}
		if len(providersNode.Content) == 0 {
			removeMapKey(authNode, "providers")
		}
		if len(authNode.Content) == 0 {
			removeMapKey(root, "auth")
		}
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
	_, err := SaveConfigPreserveCommentsWithDigest(configFile, cfg)
	return err
}

// SaveConfigPreserveCommentsWithDigest writes the config and returns the SHA-256
// of the exact bytes atomically published to configFile.
func SaveConfigPreserveCommentsWithDigest(configFile string, cfg *Config) (string, error) {
	modelsExternal := modelsJSONExists(configFile)
	persistCfg := cfg
	if modelsExternal && cfg != nil {
		copyCfg := *cfg
		copyCfg.ModelCatalog = ModelCatalogConfig{}
		copyCfg.OAuthModelAlias = nil
		persistCfg = &copyCfg
	}

	// Marshal the current cfg to YAML, then unmarshal to a yaml.Node we can merge from.
	rendered, err := yaml.Marshal(persistCfg)
	if err != nil {
		return "", err
	}
	var generated yaml.Node
	if err = yaml.Unmarshal(rendered, &generated); err != nil {
		return "", err
	}
	if generated.Kind != yaml.DocumentNode || len(generated.Content) == 0 || generated.Content[0] == nil {
		return "", fmt.Errorf("invalid generated yaml structure")
	}
	if generated.Content[0].Kind != yaml.MappingNode {
		return "", fmt.Errorf("expected generated root mapping node")
	}

	// Load original YAML as a node tree to preserve comments and ordering.
	data, err := os.ReadFile(configFile)
	if os.IsNotExist(err) {
		setConfigAPIKeyProviderEntries(generated.Content[0], persistCfg.APIKeys)
		if modelsExternal {
			removeMapKey(generated.Content[0], "oauth-model-alias")
			removeMapKey(generated.Content[0], "model-catalog")
		}
		normalizeCollectionNodeStyles(generated.Content[0])
		return writeYAMLNodeWithDigest(configFile, &generated)
	}
	if err != nil {
		return "", err
	}

	var original yaml.Node
	if err = yaml.Unmarshal(data, &original); err != nil {
		return "", err
	}
	if original.Kind != yaml.DocumentNode || len(original.Content) == 0 {
		return "", fmt.Errorf("invalid yaml document structure")
	}
	if original.Content[0] == nil || original.Content[0].Kind != yaml.MappingNode {
		return "", fmt.Errorf("expected root mapping node")
	}

	// Remove deprecated sections before merging back the sanitized config.
	removeLegacyAuthBlock(original.Content[0])
	removeLegacyOpenAICompatAPIKeys(original.Content[0])
	removeLegacyAmpKeys(original.Content[0])
	removeLegacyGenerativeLanguageKeys(original.Content[0])

	pruneMappingToGeneratedKeys(original.Content[0], generated.Content[0], "oauth-excluded-models")
	pruneMappingToGeneratedKeys(original.Content[0], generated.Content[0], "oauth-model-alias")
	pruneMappingToGeneratedKeys(original.Content[0], generated.Content[0], "oauth-request-scoped-errors")
	if modelsExternal {
		removeMapKey(original.Content[0], "model-catalog")
	} else {
		pruneMappingToGeneratedKeys(original.Content[0], generated.Content[0], "model-catalog")
	}
	pruneMappingToGeneratedKeys(original.Content[0], generated.Content[0], "api-key-rules")
	setConfigAPIKeyProviderEntries(original.Content[0], persistCfg.APIKeys)

	// Merge generated into original in-place, preserving comments/order of existing nodes.
	mergeMappingPreserve(original.Content[0], generated.Content[0])
	if modelsExternal {
		removeMapKey(original.Content[0], "oauth-model-alias")
		removeMapKey(original.Content[0], "model-catalog")
	}
	normalizeCollectionNodeStyles(original.Content[0])

	// Write back.
	return writeYAMLNodeWithDigest(configFile, &original)
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
	_, err := writeYAMLNodeWithDigest(filePath, node)
	return err
}

func writeYAMLNodeWithDigest(filePath string, node *yaml.Node) (string, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(node); err != nil {
		_ = enc.Close()
		return "", err
	}
	if err := enc.Close(); err != nil {
		return "", err
	}
	data := NormalizeCommentIndentation(buf.Bytes())
	digest := sha256.Sum256(data)
	if err := writeFileAtomically(filePath, data); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest[:]), nil
}

func writeFileAtomically(filePath string, data []byte) error {
	targetPath, err := resolveAtomicWriteTarget(filePath)
	if err != nil {
		return err
	}
	dir := filepath.Dir(targetPath)
	file, err := os.CreateTemp(dir, "."+filepath.Base(targetPath)+".tmp-*")
	if err != nil {
		return err
	}
	tempPath := file.Name()
	fileOpen := true
	defer func() {
		if fileOpen {
			_ = file.Close()
		}
		if tempPath != "" {
			_ = os.Remove(tempPath)
		}
	}()
	if err = file.Chmod(0o600); err != nil {
		return err
	}
	written, err := file.Write(data)
	if err != nil {
		return err
	}
	if written != len(data) {
		return io.ErrShortWrite
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		fileOpen = false
		return err
	}
	fileOpen = false
	if err = os.Rename(tempPath, targetPath); err != nil {
		if errors.Is(err, syscall.EBUSY) {
			return fmt.Errorf(
				"replace config file: %w; mount the config directory instead of a single config file",
				err,
			)
		}
		return err
	}
	tempPath = ""
	return syncParentDirectory(dir)
}

func syncParentDirectory(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err = directory.Sync(); err != nil {
		_ = directory.Close()
		return err
	}
	return directory.Close()
}

func resolveAtomicWriteTarget(filePath string) (string, error) {
	info, err := os.Lstat(filePath)
	switch {
	case err == nil && info.Mode()&os.ModeSymlink != 0:
		return filepath.EvalSymlinks(filePath)
	case err == nil:
		return filePath, nil
	case os.IsNotExist(err):
		return filePath, nil
	default:
		return "", err
	}
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
		_, _ = fmt.Fprintf(os.Stderr, "config: failed to ensure defaults in %s: %v\n", configFile, err)
	}
}

func modelsJSONExists(configFile string) bool {
	if configFile == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(filepath.Dir(configFile), "models.json"))
	return err == nil
}
