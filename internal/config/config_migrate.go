package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

// UnmarshalYAML 在读取旧配置时迁移亲和设置，显式的新键优先
func (routing *routingConfig) UnmarshalYAML(node *yaml.Node) error {
	type fields routingConfig
	decoded := fields(*routing)
	if err := node.Decode(&decoded); err != nil {
		return err
	}
	var keys map[string]yaml.Node
	if err := node.Decode(&keys); err != nil {
		return err
	}
	if legacy, exists := keys["claude-code-session-affinity"]; exists {
		if _, modern := keys["session-affinity"]; !modern {
			if err := legacy.Decode(&decoded.SessionAffinity); err != nil {
				return fmt.Errorf("routing.claude-code-session-affinity: %w", err)
			}
		}
		log.Info(
			"routing.claude-code-session-affinity is retired; configuration saves use routing.session-affinity",
		)
	}
	*routing = routingConfig(decoded)
	return nil
}

// UnmarshalJSON 对 JSON 配置执行相同的单向迁移，保留 encoding/json 的字段校验
func (routing *routingConfig) UnmarshalJSON(data []byte) error {
	type fields routingConfig
	decoded := fields(*routing)
	value := struct {
		*fields
		Legacy   json.RawMessage `json:"claude-code-session-affinity"`
		Affinity json.RawMessage `json:"session-affinity"`
	}{fields: &decoded}
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	raw := value.Affinity
	if len(raw) == 0 {
		raw = value.Legacy
	}
	if len(raw) != 0 {
		if err := json.Unmarshal(raw, &decoded.SessionAffinity); err != nil {
			return fmt.Errorf("routing.session-affinity: %w", err)
		}
	}
	if len(value.Legacy) != 0 {
		log.Info(
			"routing.claude-code-session-affinity is retired; configuration saves use routing.session-affinity",
		)
	}
	*routing = routingConfig(decoded)
	return nil
}

// MigrateRoutingSessionAffinityYAML 将保存内容中的旧键改为新键，保留其他字段与注释
func MigrateRoutingSessionAffinityYAML(data []byte) ([]byte, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, err
	}
	if len(document.Content) == 0 {
		return data, nil
	}
	root := document.Content[0]
	var rootFields map[string]yaml.Node
	if err := root.Decode(&rootFields); err != nil {
		return nil, err
	}
	routing, exists := rootFields["routing"]
	if !exists {
		return data, nil
	}
	var keys map[string]yaml.Node
	if err := routing.Decode(&keys); err != nil {
		return nil, err
	}
	if _, exists := keys["claude-code-session-affinity"]; !exists {
		return data, nil
	}
	var decoded routingConfig
	if err := routing.Decode(&decoded); err != nil {
		return nil, err
	}
	// 仅展开 routing 的合并字段，共用的锚点定义保持原值
	var expand func(*yaml.Node) *yaml.Node
	expand = func(node *yaml.Node) *yaml.Node {
		for node.Kind == yaml.AliasNode {
			node = node.Alias
		}
		result := *node
		result.Content = nil
		for index := 0; index+1 < len(node.Content); index += 2 {
			key := node.Content[index]
			for key.Kind == yaml.AliasNode {
				key = key.Alias
			}
			if key.Tag != "!!merge" {
				result.Content = append(result.Content, key, node.Content[index+1])
			}
		}
		for index := 0; index+1 < len(node.Content); index += 2 {
			key := node.Content[index]
			for key.Kind == yaml.AliasNode {
				key = key.Alias
			}
			if key.Tag != "!!merge" {
				continue
			}
			merged := node.Content[index+1]
			for merged.Kind == yaml.AliasNode {
				merged = merged.Alias
			}
			sources := []*yaml.Node{merged}
			if merged.Kind == yaml.SequenceNode {
				sources = merged.Content
			}
			for _, source := range sources {
				mapping := expand(source)
				for child := 0; child+1 < len(mapping.Content); child += 2 {
					if findMapKeyIndex(&result, mapping.Content[child].Value) < 0 {
						result.Content = append(
							result.Content,
							mapping.Content[child],
							mapping.Content[child+1],
						)
					}
				}
			}
		}
		return &result
	}
	normalized := expand(&routing)
	legacyIndex := findMapKeyIndex(normalized, "claude-code-session-affinity")
	if findMapKeyIndex(normalized, "session-affinity") < 0 {
		key := *normalized.Content[legacyIndex]
		key.Value = "session-affinity"
		normalized.Content[legacyIndex] = &key
	} else {
		removeMapKey(normalized, "claude-code-session-affinity")
	}
	value := getOrCreateMapValue(normalized, "session-affinity")
	updated := *value
	updated.Kind, updated.Tag, updated.Value = yaml.ScalarNode, "!!bool", fmt.Sprint(decoded.SessionAffinity)
	updated.Style = 0
	updated.Content, updated.Alias = nil, nil
	normalized.Content[findMapKeyIndex(normalized, "session-affinity")+1] = &updated
	destination := getOrCreateMapValue(root, "routing")
	normalized.Anchor = destination.Anchor
	*destination = *normalized
	var output bytes.Buffer
	encoder := yaml.NewEncoder(&output)
	encoder.SetIndent(2)
	if err := encoder.Encode(&document); err != nil {
		_ = encoder.Close()
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

// UnmarshalYAML 读取旧 Provider 密钥，显式的现行字段优先
func (cfg *Config) UnmarshalYAML(node *yaml.Node) error {
	for _, key := range []string{
		"amp-upstream-url", "amp-upstream-api-key", "amp-restrict-management-to-localhost", "amp-model-mappings",
	} {
		value, _, err := configYAMLEffectiveMappingValue(node, key, nil)
		if err != nil {
			return err
		}
		if value != nil {
			return fmt.Errorf(
				"%s is no longer supported; remove the retired Amp field and configure a supported provider",
				key,
			)
		}
	}
	legacy, _, err := configYAMLEffectiveMappingValue(node, "generative-language-api-key", nil)
	if err != nil {
		return err
	}
	var keys []string
	if legacy != nil {
		if err = legacy.Decode(&keys); err != nil {
			return errors.New(
				"generative-language-api-key must be a list of API key strings; use " +
					"gemini-api-key for structured " +
					"entries",
			)
		}
	}
	modern, _, err := configYAMLEffectiveMappingValue(node, "gemini-api-key", nil)
	if err != nil {
		return err
	}
	type configFields Config
	if err = node.Decode((*configFields)(cfg)); err != nil {
		return err
	}
	if legacy != nil && modern == nil {
		cfg.GeminiKey = nil
		for _, key := range normalizeStringSlice(keys) {
			cfg.GeminiKey = append(cfg.GeminiKey, GeminiKey{APIKey: key})
		}
	}
	return nil
}

// UnmarshalYAML 将旧 api-keys 列表读取为 APIKeyEntries
func (provider *OpenAICompatibility) UnmarshalYAML(node *yaml.Node) error {
	legacy, _, err := configYAMLEffectiveMappingValue(node, "api-keys", nil)
	if err != nil {
		return err
	}
	var keys []string
	if legacy != nil {
		if err = legacy.Decode(&keys); err != nil {
			return errors.New(
				"openai-compatibility.api-keys must be a list of API key strings; use " +
					"api-key-entries for structured " +
					"entries",
			)
		}
	}
	modern, _, err := configYAMLEffectiveMappingValue(node, "api-key-entries", nil)
	if err != nil {
		return err
	}
	type providerFields OpenAICompatibility
	if err = node.Decode((*providerFields)(provider)); err != nil {
		return err
	}
	if legacy != nil && modern == nil {
		provider.APIKeyEntries = nil
		for _, key := range normalizeStringSlice(keys) {
			provider.APIKeyEntries = append(provider.APIKeyEntries, OpenAICompatibilityAPIKey{APIKey: key})
		}
	}
	return nil
}

func removeLegacyOpenAICompatAPIKeys(root *yaml.Node) {
	if root == nil || root.Kind != yaml.MappingNode {
		return
	}
	idx := findMapKeyIndex(root, "openai-compatibility")
	if idx < 0 || idx+1 >= len(root.Content) {
		return
	}
	seq := root.Content[idx+1]
	if seq == nil || seq.Kind != yaml.SequenceNode {
		return
	}
	for i := range seq.Content {
		if seq.Content[i] != nil && seq.Content[i].Kind == yaml.MappingNode {
			removeMapKey(seq.Content[i], "api-keys")
		}
	}
}

func removeLegacyAmpKeys(root *yaml.Node) {
	if root == nil || root.Kind != yaml.MappingNode {
		return
	}
	removeMapKey(root, "amp-upstream-url")
	removeMapKey(root, "amp-upstream-api-key")
	removeMapKey(root, "amp-restrict-management-to-localhost")
	removeMapKey(root, "amp-model-mappings")
}

func removeLegacyGenerativeLanguageKeys(root *yaml.Node) {
	if root == nil || root.Kind != yaml.MappingNode {
		return
	}
	removeMapKey(root, "generative-language-api-key")
}

func removeLegacyAuthBlock(root *yaml.Node) {
	if root == nil || root.Kind != yaml.MappingNode {
		return
	}
	idx := findMapKeyIndex(root, "auth")
	if idx < 0 || idx+1 >= len(root.Content) {
		return
	}
	authNode := root.Content[idx+1]
	if authNode == nil || authNode.Kind != yaml.MappingNode {
		return
	}
	providersIdx := findMapKeyIndex(authNode, "providers")
	if providersIdx < 0 || providersIdx+1 >= len(authNode.Content) {
		removeMapKey(root, "auth")
		return
	}
	providersNode := authNode.Content[providersIdx+1]
	if providersNode == nil || providersNode.Kind != yaml.MappingNode {
		removeMapKey(root, "auth")
		return
	}
	configAPIKeyIdx := findMapKeyIndex(providersNode, "config-api-key")
	if configAPIKeyIdx < 0 {
		removeMapKey(root, "auth")
		return
	}
	for i := 0; i+1 < len(providersNode.Content); i += 2 {
		if i == configAPIKeyIdx {
			continue
		}
		removeMapKey(providersNode, providersNode.Content[i].Value)
		i -= 2
	}
}
