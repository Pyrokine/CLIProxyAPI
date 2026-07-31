package config

import (
	"errors"
	"fmt"

	"gopkg.in/yaml.v3"
)

var errManagementSecretYAMLAlias = errors.New(
	"remote-management.secret-key cannot use YAML aliases or merge keys",
)

func validateManagementSecretYAML(data []byte) error {
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil
	}
	if document.Kind != yaml.DocumentNode || len(document.Content) == 0 || document.Content[0] == nil {
		return nil
	}
	remoteManagement, merged, errLookup := configYAMLEffectiveMappingValue(
		document.Content[0],
		"remote-management",
		nil,
	)
	if errLookup != nil {
		return errLookup
	}
	if remoteManagement == nil {
		return nil
	}
	if merged || remoteManagement.Kind == yaml.AliasNode {
		return errManagementSecretYAMLAlias
	}
	secret, merged, errLookup := configYAMLEffectiveMappingValue(
		remoteManagement,
		"secret-key",
		nil,
	)
	if errLookup != nil {
		return errLookup
	}
	if secret != nil && (merged || secret.Kind == yaml.AliasNode) {
		return errManagementSecretYAMLAlias
	}
	return nil
}

func configYAMLEffectiveMappingValue(
	mapping *yaml.Node,
	key string,
	seen map[*yaml.Node]struct{},
) (*yaml.Node, bool, error) {
	if mapping == nil {
		return nil, false, nil
	}
	if seen == nil {
		seen = make(map[*yaml.Node]struct{})
	}
	if _, duplicate := seen[mapping]; duplicate {
		return nil, false, fmt.Errorf("config YAML contains a cyclic alias")
	}
	seen[mapping] = struct{}{}
	defer delete(seen, mapping)

	if mapping.Kind == yaml.AliasNode {
		if mapping.Alias == nil {
			return nil, false, fmt.Errorf("config YAML contains an unresolved alias")
		}
		value, _, errLookup := configYAMLEffectiveMappingValue(mapping.Alias, key, seen)
		return value, true, errLookup
	}
	if mapping.Kind != yaml.MappingNode {
		return nil, false, nil
	}
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		keyValue, aliased, errKey := configYAMLScalarValue(mapping.Content[index], nil)
		if errKey != nil {
			return nil, false, errKey
		}
		if keyValue == key && keyValue != "<<" {
			return mapping.Content[index+1], aliased, nil
		}
	}
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		keyValue, _, errKey := configYAMLScalarValue(mapping.Content[index], nil)
		if errKey != nil {
			return nil, false, errKey
		}
		if keyValue != "<<" {
			continue
		}
		value, found, errLookup := configYAMLMergedMappingValue(mapping.Content[index+1], key, seen)
		if errLookup != nil {
			return nil, false, errLookup
		}
		if found {
			return value, true, nil
		}
	}
	return nil, false, nil
}

func configYAMLScalarValue(
	node *yaml.Node,
	seen map[*yaml.Node]struct{},
) (string, bool, error) {
	if node == nil {
		return "", false, nil
	}
	if seen == nil {
		seen = make(map[*yaml.Node]struct{})
	}
	if _, duplicate := seen[node]; duplicate {
		return "", false, fmt.Errorf("config YAML contains a cyclic alias")
	}
	seen[node] = struct{}{}
	defer delete(seen, node)
	if node.Kind == yaml.AliasNode {
		if node.Alias == nil {
			return "", false, fmt.Errorf("config YAML contains an unresolved alias")
		}
		value, _, errResolve := configYAMLScalarValue(node.Alias, seen)
		return value, true, errResolve
	}
	if node.Kind != yaml.ScalarNode {
		return "", false, nil
	}
	return node.Value, false, nil
}

func configYAMLMergedMappingValue(
	node *yaml.Node,
	key string,
	seen map[*yaml.Node]struct{},
) (*yaml.Node, bool, error) {
	if node == nil {
		return nil, false, nil
	}
	if node.Kind == yaml.SequenceNode {
		for _, child := range node.Content {
			value, _, errLookup := configYAMLEffectiveMappingValue(child, key, seen)
			if errLookup != nil {
				return nil, false, errLookup
			}
			if value != nil {
				return value, true, nil
			}
		}
		return nil, false, nil
	}
	value, _, errLookup := configYAMLEffectiveMappingValue(node, key, seen)
	return value, value != nil, errLookup
}
