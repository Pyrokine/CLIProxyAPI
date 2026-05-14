package config

import (
	"gopkg.in/yaml.v3"
)

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
