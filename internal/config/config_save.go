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
	"strings"
	"syscall"

	"gopkg.in/yaml.v3"
)

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

// SaveConfigPreserveComments updates the YAML file while keeping its comments and key order.
func SaveConfigPreserveComments(configFile string, cfg *Config) error {
	_, err := SaveConfigPreserveCommentsWithDigest(configFile, cfg)
	return err
}

// SaveConfigPreserveCommentsWithDigest writes the config and returns the SHA-256
// of the exact bytes atomically published to configFile.
func SaveConfigPreserveCommentsWithDigest(configFile string, cfg *Config) (string, error) {
	return saveConfigPreserveComments(configFile, cfg, false)
}

func saveConfigPreserveComments(configFile string, cfg *Config, includeDefaults bool) (string, error) {
	if err := validateStorageSizeConfig(cfg); err != nil {
		return "", err
	}
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
	if persistCfg.CredentialConcurrency.LifecycleConfigRevision == 0 {
		// A lifecycle revision is assigned by Home and must be positive when supplied.
		removeMapKey(yamlMappingValue(generated.Content[0], "credential-concurrency"), "lifecycle-config-revision")
	}

	// Load original YAML as a node tree to preserve comments and ordering.
	data, err := os.ReadFile(configFile)
	if os.IsNotExist(err) {
		setConfigAPIKeyProviderEntries(generated.Content[0], persistCfg.APIKeys)
		if modelsExternal {
			removeMapKey(generated.Content[0], "oauth-model-alias")
			removeMapKey(generated.Content[0], "model-catalog")
		}
		return writeConfigYAML(configFile, &generated, persistCfg, includeDefaults)
	}
	if err != nil {
		return "", err
	}

	data, err = MigrateRoutingSessionAffinityYAML(data)
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
	pruneMappingToGeneratedKeys(original.Content[0], generated.Content[0], "oauth-settings")
	pruneMappingToGeneratedKeys(original.Content[0], generated.Content[0], "oauth-request-scoped-errors")
	if modelsExternal {
		removeMapKey(original.Content[0], "model-catalog")
	} else {
		pruneMappingToGeneratedKeys(original.Content[0], generated.Content[0], "model-catalog")
	}
	pruneMappingToGeneratedKeys(original.Content[0], generated.Content[0], "api-key-aliases")
	pruneMappingToGeneratedKeys(original.Content[0], generated.Content[0], "api-key-rules")
	setConfigAPIKeyProviderEntries(original.Content[0], persistCfg.APIKeys)

	// Merge generated into original in-place, preserving comments and order.
	if err := mergeMappingPreserve(original.Content[0], generated.Content[0], includeDefaults); err != nil {
		return "", err
	}
	prunePluginConfigMappings(original.Content[0], generated.Content[0])
	if modelsExternal {
		removeMapKey(original.Content[0], "oauth-model-alias")
		removeMapKey(original.Content[0], "model-catalog")
	}
	return writeConfigYAML(configFile, &original, persistCfg, includeDefaults)
}

type yamlDefault struct {
	key, tag, value string
}

func yamlMappingValue(mapping *yaml.Node, key string) *yaml.Node {
	if idx := findMapKeyIndex(mapping, key); idx >= 0 {
		return mapping.Content[idx+1]
	}
	return nil
}

func yamlSequenceItems(node *yaml.Node) []*yaml.Node {
	if node != nil && node.Kind == yaml.SequenceNode {
		return node.Content
	}
	return nil
}

func addMissingYAMLDefaults(mapping *yaml.Node, defaults ...yamlDefault) error {
	// An anchor can be shared with another section. Defaults belong in the local mapping.
	if mapping == nil || mapping.Kind != yaml.MappingNode || mapping.Anchor != "" {
		return nil
	}
	for _, field := range defaults {
		existing, _, err := configYAMLEffectiveMappingValue(mapping, field.key, nil)
		if err != nil {
			return err
		}
		if existing == nil {
			*getOrCreateMapValue(mapping, field.key) = yaml.Node{
				Kind: yaml.ScalarNode, Tag: field.tag, Value: field.value,
			}
		}
	}
	return nil
}

func writeConfigYAML(file string, document *yaml.Node, cfg *Config, includeDefaults bool) (string, error) {
	root := document.Content[0]
	if err := materializeYAMLDefaults(root, cfg, includeDefaults); err != nil {
		return "", err
	}
	normalizeCollectionNodeStyles(root)
	return writeYAMLNodeWithDigest(file, document)
}

func materializeYAMLDefaults(root *yaml.Node, cfg *Config, includeDefaults bool) error {
	routing := yamlMappingValue(root, "routing")
	if includeDefaults {
		if err := errors.Join(
			addMissingYAMLDefaults(routing, yamlDefault{"strategy", "!!str", "round-robin"}),
			addMissingYAMLDefaults(
				root,
				yamlDefault{"video-result-auth-cache-ttl", "!!str", "3h"},
				yamlDefault{"gpt-image-2-base-model", "!!str", "gpt-5.4-mini"},
			),
		); err != nil {
			return err
		}
	}
	if cfg.Routing.SessionAffinity {
		if err := addMissingYAMLDefaults(
			routing,
			yamlDefault{"session-affinity-ttl", "!!str", "1h"},
			yamlDefault{"session-affinity-subagents", "!!bool", "true"},
		); err != nil {
			return err
		}
	}
	if cfg.RemoteManagement.AutoCheckUpdate {
		if err := addMissingYAMLDefaults(
			yamlMappingValue(root, "remote-management"),
			yamlDefault{"check-interval", "!!int", "180"},
		); err != nil {
			return err
		}
	}
	if enabled := cfg.Antigravity.ConnectionPool.Enabled; enabled != nil && *enabled {
		pool := yamlMappingValue(yamlMappingValue(root, "antigravity"), "connection-pool")
		if err := addMissingYAMLDefaults(pool, yamlDefault{"max-idle-conns-per-host", "!!int", "2"}); err != nil {
			return err
		}
	}
	return materializeYAMLRecordDefaults(root)
}

// yamlRecordDefaults covers fixed values on existing records, not inherited overrides.
func yamlRecordDefaults(path []string) []yamlDefault {
	if len(path) == 3 && path[0] == "api-key-rules" && path[2] == "limits" {
		return []yamlDefault{{"requests", "!!int", "0"}, {"tokens", "!!int", "0"}, {"cost-micro", "!!int", "0"}}
	}
	priority := yamlDefault{"priority", "!!int", "0"}
	websockets := yamlDefault{"websockets", "!!bool", "false"}
	forceMapping := yamlDefault{"force-mapping", "!!bool", "false"}
	compatibility := yamlDefault{"is-compat", "!!bool", "false"}
	switch strings.Join(path, ".") {
	case "gemini-api-key", "interactions-api-key", "meta-api-key", "vertex-api-key":
		return []yamlDefault{priority}
	case "claude-api-key":
		return []yamlDefault{priority, {"rebuild-mid-system-message", "!!bool", "false"}}
	case "xai-api-key":
		return []yamlDefault{priority, websockets}
	case "codex-api-key":
		return []yamlDefault{priority, websockets, {"alpha-search", "!!bool", "false"}}
	case "openai-compatibility":
		return []yamlDefault{priority, {"disabled", "!!bool", "false"}, {"support-prompt-cache-key", "!!bool", "false"}}
	case "openai-compatibility.api-key-entries":
		return []yamlDefault{{"disabled", "!!bool", "false"}}
	case "gemini-api-key.models", "interactions-api-key.models", "claude-api-key.models", "xai-api-key.models", "meta-api-key.models":
		return []yamlDefault{forceMapping, compatibility}
	case "vertex-api-key.models":
		return []yamlDefault{forceMapping}
	case "codex-api-key.models":
		return []yamlDefault{forceMapping, compatibility, {"support-configuration-update", "!!bool", "false"}}
	case "openai-compatibility.models":
		return []yamlDefault{forceMapping, compatibility, {"image", "!!bool", "false"}}
	case "plugins.store-auth":
		return []yamlDefault{{"allow-insecure", "!!bool", "false"}}
	}
	return nil
}

func materializeYAMLRecordDefaults(root *yaml.Node) error {
	for _, family := range []string{
		"gemini-api-key", "interactions-api-key", "codex-api-key", "xai-api-key", "meta-api-key",
		"claude-api-key", "vertex-api-key", "openai-compatibility",
	} {
		providerDefaults := yamlRecordDefaults([]string{family})
		modelDefaults := yamlRecordDefaults([]string{family, "models"})
		keyDefaults := yamlRecordDefaults([]string{family, "api-key-entries"})
		for _, entry := range yamlSequenceItems(yamlMappingValue(root, family)) {
			if family == "codex-api-key" && isInactiveCodexYAMLRow(entry) ||
				family == "claude-api-key" && mappingScalarValue(entry, "api-key") == "" && mappingScalarValue(
					entry, "base-url",
				) == "" {
				continue
			}
			if err := addMissingYAMLDefaults(entry, providerDefaults...); err != nil {
				return err
			}
			for _, model := range yamlSequenceItems(yamlMappingValue(entry, "models")) {
				if err := addMissingYAMLDefaults(model, modelDefaults...); err != nil {
					return err
				}
			}
			for _, key := range yamlSequenceItems(yamlMappingValue(entry, "api-key-entries")) {
				if err := addMissingYAMLDefaults(key, keyDefaults...); err != nil {
					return err
				}
			}
		}
	}
	storeAuthPath := []string{"plugins", "store-auth"}
	inactiveStoreRule := inactiveConfigYAMLRow(storeAuthPath)
	storeAuthDefaults := yamlRecordDefaults(storeAuthPath)
	for _, rule := range yamlSequenceItems(yamlMappingValue(yamlMappingValue(root, "plugins"), "store-auth")) {
		if !inactiveStoreRule(rule) {
			if err := addMissingYAMLDefaults(rule, storeAuthDefaults...); err != nil {
				return err
			}
		}
	}
	if rules := yamlMappingValue(root, "api-key-rules"); rules != nil && rules.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(rules.Content); i += 2 {
			rule := rules.Content[i+1]
			limits := yamlSequenceItems(yamlMappingValue(rule, "limits"))
			if len(limits) == 0 {
				continue
			}
			if err := addMissingYAMLDefaults(
				rule,
				yamlDefault{"policy-profile", "!!str", "default"},
				yamlDefault{"reservation-ttl", "!!str", "24h"},
			); err != nil {
				return err
			}
			for _, limit := range limits {
				if err := addMissingYAMLDefaults(
					limit, yamlRecordDefaults([]string{"api-key-rules", rules.Content[i].Value, "limits"})...,
				); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// SaveConfigPreserveCommentsUpdateNestedScalar updates a nested scalar key path like ["a","b"]
// while preserving comments and positions.
func SaveConfigPreserveCommentsUpdateNestedScalar(configFile string, path []string, value string) error {
	data, err := os.ReadFile(configFile)
	if err != nil {
		return err
	}
	data, err = MigrateRoutingSessionAffinityYAML(data)
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
			node = getOrCreateMapValue(node, key)
		}
	}
	return writeYAMLNode(configFile, &root)
}

// writeYAMLNode encodes a yaml.Node to the given file.
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
	data := buf.Bytes()
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

// EnsureDefaultsInFile writes applicable local defaults during startup.
// Inherited or externally managed values keep their existing source.
func EnsureDefaultsInFile(configFile string, cfg *Config) {
	if configFile == "" || cfg == nil {
		return
	}
	if _, err := saveConfigPreserveComments(configFile, cfg, true); err != nil {
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
