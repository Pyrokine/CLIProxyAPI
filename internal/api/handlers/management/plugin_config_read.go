package management

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/pluginhost"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/pluginstore"
	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"
)

// GetPluginConfig returns the preserved editable configuration object.
func (h *Handler) GetPluginConfig(c *gin.Context) {
	id, ok := pluginIDFromRequest(c)
	if !ok {
		return
	}
	if h == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "plugin_not_found", "message": "plugin not found"})
		return
	}
	h.mu.Lock()
	snapshot := pluginRuntimeConfigSnapshot(h.cfg)
	host := h.pluginHost
	h.mu.Unlock()
	if snapshot == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "plugin_not_found", "message": "plugin not found"})
		return
	}
	configs, err := redactedPluginConfigs(snapshot)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "plugin_config_encode_failed"})
		return
	}
	if _, item, configured := pluginConfigByID(configs, id); configured {
		body, err := pluginConfigJSONObject(item)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "plugin_config_encode_failed"})
			return
		}
		c.JSON(http.StatusOK, body)
		return
	}
	if host != nil {
		for _, entry := range host.RegisteredPlugins() {
			if pluginstore.CanonicalPluginID(entry.ID) == id {
				c.JSON(http.StatusOK, gin.H{})
				return
			}
		}
	}
	dir, err := config.ResolvePluginsDir(snapshot.Plugins.Dir)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "plugin_directory_invalid"})
		return
	}
	files, err := pluginhost.DiscoverPluginFiles(dir)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "plugin_discovery_failed"})
		return
	}
	for _, file := range files {
		if pluginstore.CanonicalPluginID(file.ID) == id {
			c.JSON(http.StatusOK, gin.H{})
			return
		}
	}
	c.JSON(http.StatusNotFound, gin.H{"error": "plugin_not_found", "message": "plugin not found"})
}

func redactedPluginConfigs(snapshot *config.Config) (map[string]config.PluginInstanceConfig, error) {
	snapshot.NormalizePluginsConfig()
	data, err := yaml.Marshal(snapshot)
	if err != nil {
		return nil, err
	}
	data, err = redactConfigYAML(data)
	if err != nil {
		return nil, err
	}
	var redacted config.Config
	if err := yaml.Unmarshal(data, &redacted); err != nil {
		return nil, err
	}
	return redacted.Plugins.Configs, nil
}

func pluginConfigJSONObject(item config.PluginInstanceConfig) (map[string]any, error) {
	value, err := yamlNodeToJSONValue(pluginConfigNode(item))
	if err != nil {
		return nil, err
	}
	body, ok := value.(map[string]any)
	if !ok || body == nil {
		return map[string]any{}, nil
	}
	delete(body, "store")
	if _, err := json.Marshal(body); err != nil {
		return nil, err
	}
	return body, nil
}

func yamlNodeToJSONValue(node *yaml.Node) (any, error) {
	active := make(map[*yaml.Node]bool)
	var convert func(*yaml.Node) (any, error)
	convert = func(node *yaml.Node) (any, error) {
		if node == nil {
			return nil, nil
		}
		if active[node] {
			return nil, fmt.Errorf("cyclic YAML alias")
		}
		active[node] = true
		defer delete(active, node)
		switch node.Kind {
		case yaml.MappingNode:
			out := make(map[string]any, len(node.Content)/2)
			for i := 0; i+1 < len(node.Content); i += 2 {
				key := node.Content[i]
				if key == nil {
					continue
				}
				if _, exists := out[key.Value]; exists {
					return nil, fmt.Errorf("duplicate YAML field %q", key.Value)
				}
				value, err := convert(node.Content[i+1])
				if err != nil {
					return nil, err
				}
				out[key.Value] = value
			}
			return out, nil
		case yaml.SequenceNode:
			out := make([]any, 0, len(node.Content))
			for _, child := range node.Content {
				value, err := convert(child)
				if err != nil {
					return nil, err
				}
				out = append(out, value)
			}
			return out, nil
		case yaml.AliasNode:
			return convert(node.Alias)
		case yaml.ScalarNode:
			if node.Tag == "!!float" && json.Valid([]byte(node.Value)) {
				return json.Number(node.Value), nil
			}
			var value any
			if err := node.Decode(&value); err != nil {
				return nil, err
			}
			return value, nil
		default:
			return nil, fmt.Errorf("unsupported YAML node kind %d", node.Kind)
		}
	}
	return convert(node)
}

func restorePluginConfigPlaceholders(
	current *config.Config,
	id string,
	candidate config.PluginInstanceConfig,
) (config.PluginInstanceConfig, error) {
	data, err := yaml.Marshal(candidate)
	if err != nil {
		return config.PluginInstanceConfig{}, err
	}
	hasPlaceholder, err := configYAMLContainsPlaceholder(data)
	if err != nil {
		return config.PluginInstanceConfig{}, err
	}
	if !hasPlaceholder {
		return candidate, nil
	}
	source, err := yaml.Marshal(current)
	if err != nil {
		return config.PluginInstanceConfig{}, err
	}
	sourceDocument, err := decodeConfigYAML(source)
	if err != nil {
		return config.PluginInstanceConfig{}, err
	}
	plan, err := newConfigYAMLRedactionPlan(sourceDocument)
	if err != nil {
		return config.PluginInstanceConfig{}, err
	}
	redacted, err := redactedPluginConfigs(pluginRuntimeConfigSnapshot(current))
	if err != nil {
		return config.PluginInstanceConfig{}, err
	}
	_, publicItem, _ := pluginConfigByID(redacted, id)
	publicBody, err := pluginConfigJSONObject(publicItem)
	if err != nil {
		return config.PluginInstanceConfig{}, err
	}
	publicJSON, err := json.Marshal(publicBody)
	if err != nil {
		return config.PluginInstanceConfig{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(publicJSON))
	decoder.UseNumber()
	if err := decoder.Decode(&publicBody); err != nil {
		return config.PluginInstanceConfig{}, err
	}
	publicNode, err := yamlNodeFromJSONObject(publicBody)
	if err != nil {
		return config.PluginInstanceConfig{}, err
	}
	publicItem, err = pluginInstanceConfigFromNode(publicNode)
	if err != nil {
		return config.PluginInstanceConfig{}, err
	}
	publicConfig := pluginRuntimeConfigSnapshot(current)
	setPluginConfigByID(publicConfig.Plugins.Configs, id, publicItem)
	publicData, err := yaml.Marshal(publicConfig)
	if err != nil {
		return config.PluginInstanceConfig{}, err
	}
	publicDocument, err := decodeConfigYAML(publicData)
	if err != nil {
		return config.PluginInstanceConfig{}, err
	}
	allowed := make(map[string]*configYAMLSecret)
	err = visitConfigYAMLScalarContexts(
		publicDocument, func(node *yaml.Node, ordinal, semantic string, _ bool) error {
			original := plan.secretsByPlaceholder[node.Value]
			if original == nil {
				return nil
			}
			secret := allowed[node.Value]
			if secret == nil {
				copied := *original
				copied.paths = make(map[string]struct{})
				secret = &copied
				allowed[node.Value] = secret
			}
			secret.paths[configYAMLOrdinalLocation(ordinal)] = struct{}{}
			secret.paths[configYAMLSemanticLocation(semantic)] = struct{}{}
			return nil
		},
	)
	if err != nil {
		return config.PluginInstanceConfig{}, err
	}
	next := pluginRuntimeConfigSnapshot(current)
	setPluginConfigByID(next.Plugins.Configs, id, candidate)
	target, err := yaml.Marshal(next)
	if err != nil {
		return config.PluginInstanceConfig{}, err
	}
	document, err := decodeConfigYAML(target)
	if err != nil {
		return config.PluginInstanceConfig{}, err
	}
	resolved := make(map[string]*yaml.Node)
	anchors := make(map[string]*yaml.Node)
	err = visitConfigYAMLNodeContexts(
		document, func(node *yaml.Node, ordinal, semantic string, sensitive bool) error {
			if node.Anchor != "" {
				anchors[node.Anchor] = node
			}
			if node.Kind == yaml.ScalarNode && !isConfigYAMLPlaceholder(node.Value) {
				for marker, secret := range allowed {
					if node.Value == secret.value && node.Tag == secret.tag && configYAMLSecretMatchesLocation(
						secret, ordinal, semantic, sensitive,
					) {
						resolved[marker] = node
					}
				}
			}
			return nil
		},
	)
	if err != nil {
		return config.PluginInstanceConfig{}, err
	}
	err = visitConfigYAMLScalarContexts(
		document, func(node *yaml.Node, ordinal, semantic string, sensitive bool) error {
			if !isConfigYAMLPlaceholder(node.Value) {
				return nil
			}
			marker := node.Value
			secret := allowed[marker]
			if secret == nil || !configYAMLSecretMatchesLocation(secret, ordinal, semantic, sensitive) {
				return fmt.Errorf("stale or misplaced redacted plugin value at %s", ordinal)
			}
			first := resolved[marker]
			if first == nil {
				node.Value, node.Tag, node.Style = secret.value, secret.tag, secret.style
				resolved[marker] = node
				return nil
			}
			if first.Anchor == "" {
				name := secret.node.Anchor
				if name == "" {
					name = "cpa_plugin_value_" + strings.TrimSuffix(
						strings.TrimPrefix(
							marker, configYAMLPlaceholderPrefix,
						), "__",
					)
				}
				for anchors[name] != nil && anchors[name] != first {
					name += "_"
				}
				first.Anchor = name
				anchors[name] = first
			}
			*node = yaml.Node{Kind: yaml.AliasNode, Value: first.Anchor, Alias: first}
			return nil
		},
	)
	if err != nil {
		return config.PluginInstanceConfig{}, err
	}
	var restored config.Config
	if err := document.Decode(&restored); err != nil {
		return config.PluginInstanceConfig{}, err
	}
	_, item, _ := pluginConfigByID(restored.Plugins.Configs, id)
	return item, nil
}
