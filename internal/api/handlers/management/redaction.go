package management

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"
)

var sensitiveMapKeyParents = map[string]struct{}{
	"api-key-aliases": {},
	"api-key-rules":   {},
	"apis":            {},
}

func writeRedactedJSON(c *gin.Context, status int, payload any) {
	redacted, err := redactedManagementPayload(payload)
	if err != nil {
		c.JSON(500, gin.H{"error": "failed to redact management response"})
		return
	}
	c.JSON(status, redacted)
}

func redactedManagementPayload(payload any) (any, error) {
	return redactedManagementPayloadContext(context.Background(), payload)
}

func redactedManagementPayloadContext(ctx context.Context, payload any) (any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal management payload: %w", err)
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	data = util.MaskSensitivePayload(data)
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var decoded any
	if err = decoder.Decode(&decoded); err != nil {
		return nil, fmt.Errorf("decode redacted management payload: %w", err)
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	var extra any
	if err = decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			err = errors.New("unexpected trailing JSON value")
		}
		return nil, fmt.Errorf("decode redacted management payload: %w", err)
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return decoded, nil
}

func shouldRedactMapKey(parent string) bool {
	_, ok := sensitiveMapKeyParents[parent]
	return ok
}

const (
	configYAMLPlaceholderPrefix       = "__CLI_PROXY_CONFIG_SECRET_"
	configYAMLAnchorPlaceholderPrefix = "cpa_config_anchor_"
)

type configYAMLSecret struct {
	node        *yaml.Node
	value       string
	tag         string
	style       yaml.Style
	placeholder string
	firstPath   string
	paths       map[string]struct{}
}

type configYAMLAnchor struct {
	node        *yaml.Node
	value       string
	placeholder string
	firstPath   string
	paths       map[string]struct{}
}

type configYAMLRedactionPlan struct {
	placeholderKey       [32]byte
	secrets              []*configYAMLSecret
	secretsByNode        map[*yaml.Node]*configYAMLSecret
	secretsByPlaceholder map[string]*configYAMLSecret
	anchors              []*configYAMLAnchor
	anchorsByPlaceholder map[string]*configYAMLAnchor
	sourceScalars        map[string]struct{}
	sourceAnchors        map[string]struct{}
	placeholders         map[string]struct{}
	anchorPlaceholders   map[string]struct{}
}

var configYAMLPlaceholderKey = newConfigYAMLPlaceholderKey()

func newConfigYAMLPlaceholderKey() [32]byte {
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		panic(fmt.Sprintf("initialize config YAML redaction key: %v", err))
	}
	return key
}

func redactConfigYAML(data []byte) ([]byte, error) {
	return redactConfigYAMLWithKey(data, configYAMLPlaceholderKey)
}

func redactConfigYAMLWithKey(data []byte, placeholderKey [32]byte) ([]byte, error) {
	document, err := decodeConfigYAML(data)
	if err != nil {
		return nil, err
	}
	plan, err := newConfigYAMLRedactionPlanWithKey(document, placeholderKey)
	if err != nil {
		return nil, err
	}
	for _, secret := range plan.secrets {
		secret.node.Value = secret.placeholder
		secret.node.Tag = "!!str"
	}
	for _, anchor := range plan.anchors {
		anchor.node.Anchor = anchor.placeholder
	}
	syncConfigYAMLAliasValues(document, make(map[*yaml.Node]struct{}))
	return encodeConfigYAML(document)
}

func restoreConfigYAMLPlaceholders(data []byte, source []byte) ([]byte, error) {
	return restoreConfigYAMLPlaceholdersWithKey(data, source, configYAMLPlaceholderKey)
}

func restoreConfigYAMLPlaceholdersWithKey(
	data []byte,
	source []byte,
	placeholderKey [32]byte,
) ([]byte, error) {
	sourceDocument, err := decodeConfigYAML(source)
	if err != nil {
		return nil, fmt.Errorf("decode current config YAML: %w", err)
	}
	plan, err := newConfigYAMLRedactionPlanWithKey(sourceDocument, placeholderKey)
	if err != nil {
		return nil, err
	}
	document, err := decodeConfigYAML(data)
	if err != nil {
		return nil, err
	}

	usedPlaceholders := make(map[string]*yaml.Node)
	restoreNodes := make(map[*yaml.Node]*configYAMLSecret)
	err = visitConfigYAMLScalarContexts(
		document,
		func(
			node *yaml.Node,
			ordinalPath string,
			semanticPath string,
			sensitive bool,
		) error {
			if !isConfigYAMLPlaceholder(node.Value) {
				return nil
			}
			expected := plan.secretsByPlaceholder[node.Value]
			if expected == nil {
				return fmt.Errorf("stale or misplaced redacted value at %s", ordinalPath)
			}
			_, semanticMatch := expected.paths[configYAMLSemanticLocation(semanticPath)]
			_, ordinalMatch := expected.paths[configYAMLOrdinalLocation(ordinalPath)]
			if !semanticMatch && !(sensitive && ordinalMatch) {
				return fmt.Errorf("stale or misplaced redacted value at %s", ordinalPath)
			}
			if previous := usedPlaceholders[node.Value]; previous != nil && previous != node {
				return fmt.Errorf("duplicated redacted value at %s", ordinalPath)
			}
			usedPlaceholders[node.Value] = node
			restoreNodes[node] = expected
			return nil
		},
	)
	if err != nil {
		return nil, err
	}

	usedAnchorPlaceholders := make(map[string]*yaml.Node)
	restoreAnchors := make(map[*yaml.Node]*configYAMLAnchor)
	err = visitConfigYAMLNodeContexts(
		document,
		func(node *yaml.Node, ordinalPath string, semanticPath string, _ bool) error {
			if !isConfigYAMLAnchorPlaceholder(node.Anchor) {
				return nil
			}
			expected := plan.anchorsByPlaceholder[node.Anchor]
			if expected == nil {
				return fmt.Errorf("stale redacted anchor at %s", ordinalPath)
			}
			_, semanticMatch := expected.paths[configYAMLSemanticLocation(semanticPath)]
			_, ordinalMatch := expected.paths[configYAMLOrdinalLocation(ordinalPath)]
			if !semanticMatch && !ordinalMatch {
				return fmt.Errorf("stale or misplaced redacted anchor at %s", ordinalPath)
			}
			if previous := usedAnchorPlaceholders[node.Anchor]; previous != nil && previous != node {
				return fmt.Errorf("duplicated redacted anchor at %s", ordinalPath)
			}
			usedAnchorPlaceholders[node.Anchor] = node
			restoreAnchors[node] = expected
			return nil
		},
	)
	if err != nil {
		return nil, err
	}

	for node, secret := range restoreNodes {
		node.Value = secret.value
		node.Tag = secret.tag
		node.Style = secret.style
	}
	for node, anchor := range restoreAnchors {
		node.Anchor = anchor.value
	}
	syncConfigYAMLAliasValues(document, make(map[*yaml.Node]struct{}))
	return encodeConfigYAML(document)
}

func configYAMLOrdinalLocation(path string) string {
	return "ordinal:" + path
}

func configYAMLSemanticLocation(path string) string {
	return "semantic:" + path
}

func configYAMLContainsPlaceholder(data []byte) (bool, error) {
	document, err := decodeConfigYAML(data)
	if err != nil {
		return false, err
	}
	containsPlaceholder := false
	err = visitConfigYAMLScalarNodes(
		document,
		"$",
		make(map[*yaml.Node]struct{}),
		func(node *yaml.Node, _ string) error {
			containsPlaceholder = containsPlaceholder || isConfigYAMLPlaceholder(node.Value)
			return nil
		},
	)
	return containsPlaceholder, err
}

func decodeConfigYAML(data []byte) (*yaml.Node, error) {
	var document yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode config YAML: %w", err)
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			err = errors.New("unexpected trailing YAML document")
		}
		return nil, fmt.Errorf("decode config YAML: %w", err)
	}
	return &document, nil
}

func encodeConfigYAML(document *yaml.Node) ([]byte, error) {
	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	if err := encoder.Encode(document); err != nil {
		return nil, fmt.Errorf("encode config YAML: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("encode config YAML: %w", err)
	}
	return out.Bytes(), nil
}

func newConfigYAMLRedactionPlan(document *yaml.Node) (*configYAMLRedactionPlan, error) {
	return newConfigYAMLRedactionPlanWithKey(document, configYAMLPlaceholderKey)
}

func newConfigYAMLRedactionPlanWithKey(
	document *yaml.Node,
	placeholderKey [32]byte,
) (*configYAMLRedactionPlan, error) {
	plan := &configYAMLRedactionPlan{
		placeholderKey:       placeholderKey,
		secretsByNode:        make(map[*yaml.Node]*configYAMLSecret),
		secretsByPlaceholder: make(map[string]*configYAMLSecret),
		anchorsByPlaceholder: make(map[string]*configYAMLAnchor),
		sourceScalars:        make(map[string]struct{}),
		sourceAnchors:        make(map[string]struct{}),
		placeholders:         make(map[string]struct{}),
		anchorPlaceholders:   make(map[string]struct{}),
	}
	collectConfigYAMLScalars(document, plan.sourceScalars, make(map[*yaml.Node]struct{}))
	semanticPathCounts := make(map[string]int)
	anchorsByNode := make(map[*yaml.Node]*configYAMLAnchor)
	if err := visitConfigYAMLNodeContexts(
		document,
		func(node *yaml.Node, ordinalPath string, semanticPath string, _ bool) error {
			semanticPathCounts[semanticPath]++
			if node.Anchor == "" {
				return nil
			}
			plan.sourceAnchors[node.Anchor] = struct{}{}
			anchor := anchorsByNode[node]
			if anchor == nil {
				anchor = &configYAMLAnchor{
					node:      node,
					value:     node.Anchor,
					firstPath: ordinalPath,
					paths:     make(map[string]struct{}),
				}
				anchorsByNode[node] = anchor
				plan.anchors = append(plan.anchors, anchor)
			}
			anchor.paths[configYAMLOrdinalLocation(ordinalPath)] = struct{}{}
			anchor.paths[configYAMLSemanticLocation(semanticPath)] = struct{}{}
			return nil
		},
	); err != nil {
		return nil, err
	}
	for _, anchor := range plan.anchors {
		for path := range anchor.paths {
			if !strings.HasPrefix(path, "semantic:") {
				continue
			}
			if semanticPathCounts[strings.TrimPrefix(path, "semantic:")] > 1 {
				delete(anchor.paths, path)
			}
		}
	}
	if err := visitConfigYAMLSecretsWithPaths(
		document,
		func(node *yaml.Node, ordinalPath string, semanticPath string) error {
			if strings.TrimSpace(node.Value) == "" {
				return nil
			}
			secret := plan.secretsByNode[node]
			if secret == nil {
				secret = &configYAMLSecret{
					node:      node,
					value:     node.Value,
					tag:       node.Tag,
					style:     node.Style,
					firstPath: semanticPath,
					paths:     make(map[string]struct{}),
				}
				plan.secrets = append(plan.secrets, secret)
				plan.secretsByNode[node] = secret
			}
			secret.paths[configYAMLOrdinalLocation(ordinalPath)] = struct{}{}
			secret.paths[configYAMLSemanticLocation(semanticPath)] = struct{}{}
			return nil
		},
	); err != nil {
		return nil, err
	}
	if err := visitConfigYAMLScalarContexts(
		document,
		func(node *yaml.Node, ordinalPath string, semanticPath string, _ bool) error {
			secret := plan.secretsByNode[node]
			if secret == nil {
				return nil
			}
			secret.paths[configYAMLOrdinalLocation(ordinalPath)] = struct{}{}
			secret.paths[configYAMLSemanticLocation(semanticPath)] = struct{}{}
			return nil
		},
	); err != nil {
		return nil, err
	}
	for _, secret := range plan.secrets {
		secret.placeholder = plan.newPlaceholder(secret)
		plan.secretsByPlaceholder[secret.placeholder] = secret
	}
	for _, anchor := range plan.anchors {
		anchor.placeholder = plan.newAnchorPlaceholder(anchor)
		plan.anchorsByPlaceholder[anchor.placeholder] = anchor
	}
	return plan, nil
}

func (p *configYAMLRedactionPlan) newPlaceholder(secret *configYAMLSecret) string {
	for salt := 0; ; salt++ {
		mac := hmac.New(sha256.New, p.placeholderKey[:])
		_, _ = mac.Write([]byte(secret.firstPath))
		_, _ = mac.Write([]byte{0})
		_, _ = mac.Write([]byte(secret.value))
		_, _ = mac.Write([]byte{0})
		_, _ = mac.Write([]byte(strconv.Itoa(salt)))
		digest := mac.Sum(nil)
		placeholder := configYAMLPlaceholderPrefix + hex.EncodeToString(digest[:16]) + "__"
		if _, exists := p.sourceScalars[placeholder]; exists {
			continue
		}
		if _, exists := p.placeholders[placeholder]; exists {
			continue
		}
		p.placeholders[placeholder] = struct{}{}
		return placeholder
	}
}

func (p *configYAMLRedactionPlan) newAnchorPlaceholder(anchor *configYAMLAnchor) string {
	for salt := 0; ; salt++ {
		mac := hmac.New(sha256.New, p.placeholderKey[:])
		_, _ = mac.Write([]byte("anchor"))
		_, _ = mac.Write([]byte{0})
		_, _ = mac.Write([]byte(anchor.firstPath))
		_, _ = mac.Write([]byte{0})
		_, _ = mac.Write([]byte(anchor.value))
		_, _ = mac.Write([]byte{0})
		_, _ = mac.Write([]byte(strconv.Itoa(salt)))
		digest := mac.Sum(nil)
		placeholder := configYAMLAnchorPlaceholderPrefix + hex.EncodeToString(digest[:16])
		if _, exists := p.sourceAnchors[placeholder]; exists {
			continue
		}
		if _, exists := p.anchorPlaceholders[placeholder]; exists {
			continue
		}
		p.anchorPlaceholders[placeholder] = struct{}{}
		return placeholder
	}
}

func isConfigYAMLPlaceholder(value string) bool {
	if !strings.HasPrefix(value, configYAMLPlaceholderPrefix) || !strings.HasSuffix(value, "__") {
		return false
	}
	digest := strings.TrimSuffix(strings.TrimPrefix(value, configYAMLPlaceholderPrefix), "__")
	if len(digest) != 32 {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}

func isConfigYAMLAnchorPlaceholder(value string) bool {
	if !strings.HasPrefix(value, configYAMLAnchorPlaceholderPrefix) {
		return false
	}
	digest := strings.TrimPrefix(value, configYAMLAnchorPlaceholderPrefix)
	if len(digest) != 32 {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}

func visitConfigYAMLNodes(
	node *yaml.Node,
	path string,
	seen map[*yaml.Node]struct{},
	visit func(*yaml.Node, string) error,
) error {
	if node == nil {
		return nil
	}
	if _, ok := seen[node]; ok {
		return nil
	}
	seen[node] = struct{}{}
	if err := visit(node, path); err != nil {
		return err
	}
	for index, child := range node.Content {
		if err := visitConfigYAMLNodes(
			child,
			fmt.Sprintf("%s/content%d", path, index),
			seen,
			visit,
		); err != nil {
			return err
		}
	}
	if node.Kind == yaml.AliasNode {
		return visitConfigYAMLNodes(node.Alias, path+"/alias", seen, visit)
	}
	return nil
}

func syncConfigYAMLAliasValues(node *yaml.Node, seen map[*yaml.Node]struct{}) {
	if node == nil {
		return
	}
	if _, ok := seen[node]; ok {
		return
	}
	seen[node] = struct{}{}
	if node.Kind == yaml.AliasNode && node.Alias != nil {
		node.Value = node.Alias.Anchor
	}
	for _, child := range node.Content {
		syncConfigYAMLAliasValues(child, seen)
	}
}

func collectConfigYAMLScalars(node *yaml.Node, values map[string]struct{}, seen map[*yaml.Node]struct{}) {
	if node == nil {
		return
	}
	if _, ok := seen[node]; ok {
		return
	}
	seen[node] = struct{}{}
	if node.Kind == yaml.ScalarNode {
		values[node.Value] = struct{}{}
	}
	if node.Kind == yaml.AliasNode {
		collectConfigYAMLScalars(node.Alias, values, seen)
	}
	for _, child := range node.Content {
		collectConfigYAMLScalars(child, values, seen)
	}
}

func visitConfigYAMLScalarNodes(
	node *yaml.Node,
	path string,
	stack map[*yaml.Node]struct{},
	visit func(*yaml.Node, string) error,
) error {
	if node == nil {
		return nil
	}
	if _, ok := stack[node]; ok {
		return nil
	}
	stack[node] = struct{}{}

	switch node.Kind {
	case yaml.DocumentNode:
		for index, child := range node.Content {
			if err := visitConfigYAMLScalarNodes(
				child,
				fmt.Sprintf("%s/document%d", path, index),
				stack,
				visit,
			); err != nil {
				return err
			}
		}
	case yaml.MappingNode:
		for index := 0; index+1 < len(node.Content); index += 2 {
			entryPath := fmt.Sprintf("%s/mapping%d", path, index/2)
			if err := visitConfigYAMLScalarNodes(node.Content[index], entryPath+"/key", stack, visit); err != nil {
				return err
			}
			if err := visitConfigYAMLScalarNodes(node.Content[index+1], entryPath+"/value", stack, visit); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		for index, child := range node.Content {
			if err := visitConfigYAMLScalarNodes(
				child,
				fmt.Sprintf("%s/sequence%d", path, index),
				stack,
				visit,
			); err != nil {
				return err
			}
		}
	case yaml.AliasNode:
		return visitConfigYAMLScalarNodes(node.Alias, path+"/alias", stack, visit)
	case yaml.ScalarNode:
		return visit(node, path)
	}
	return nil
}

type configYAMLContextVisitState struct {
	node               *yaml.Node
	fieldName          string
	inheritedSensitive bool
}

func visitConfigYAMLSecrets(document *yaml.Node, visit func(*yaml.Node, string) error) error {
	return visitConfigYAMLSecretsWithPaths(
		document,
		func(node *yaml.Node, ordinalPath string, _ string) error {
			return visit(node, ordinalPath)
		},
	)
}

func visitConfigYAMLSecretsWithPaths(
	document *yaml.Node,
	visit func(*yaml.Node, string, string) error,
) error {
	return visitConfigYAMLScalarContexts(
		document,
		func(
			node *yaml.Node,
			ordinalPath string,
			semanticPath string,
			sensitive bool,
		) error {
			if !sensitive {
				return nil
			}
			return visit(node, ordinalPath, semanticPath)
		},
	)
}

func visitConfigYAMLScalarContexts(
	document *yaml.Node,
	visit func(*yaml.Node, string, string, bool) error,
) error {
	return visitConfigYAMLNodeContexts(
		document,
		func(node *yaml.Node, ordinalPath string, semanticPath string, sensitive bool) error {
			if node.Kind != yaml.ScalarNode {
				return nil
			}
			return visit(node, ordinalPath, semanticPath, sensitive)
		},
	)
}

func visitConfigYAMLNodeContexts(
	document *yaml.Node,
	visit func(*yaml.Node, string, string, bool) error,
) error {
	return visitConfigYAMLContextNode(
		document,
		"",
		"$",
		"$",
		false,
		make(map[configYAMLContextVisitState]struct{}),
		visit,
	)
}

func visitConfigYAMLContextNode(
	node *yaml.Node,
	fieldName string,
	ordinalPath string,
	semanticPath string,
	inheritedSensitive bool,
	seen map[configYAMLContextVisitState]struct{},
	visit func(*yaml.Node, string, string, bool) error,
) error {
	if node == nil {
		return nil
	}
	state := configYAMLContextVisitState{
		node:               node,
		fieldName:          fieldName,
		inheritedSensitive: inheritedSensitive,
	}
	if _, ok := seen[state]; ok {
		return nil
	}
	seen[state] = struct{}{}

	valueSensitive := inheritedSensitive || isValueSensitiveConfigField(fieldName)
	if err := visit(node, ordinalPath, semanticPath, valueSensitive); err != nil {
		return err
	}
	switch node.Kind {
	case yaml.DocumentNode:
		for index, child := range node.Content {
			if err := visitConfigYAMLContextNode(
				child,
				fieldName,
				fmt.Sprintf("%s/document%d", ordinalPath, index),
				semanticPath+"/document",
				valueSensitive,
				seen,
				visit,
			); err != nil {
				return err
			}
		}
	case yaml.MappingNode:
		redactMapKeys := shouldRedactMapKey(fieldName)
		redactKeys := valueSensitive || redactMapKeys
		for index := 0; index+1 < len(node.Content); index += 2 {
			entryOrdinalPath := fmt.Sprintf("%s/mapping%d", ordinalPath, index/2)
			keyNode := node.Content[index]
			valueNode := node.Content[index+1]
			_, childFieldName, hasFieldName := yamlSemanticScalar(keyNode)
			entrySemanticPath := semanticPath + "/mapping@" + configYAMLSemanticMapKey(keyNode, redactKeys)
			if !hasFieldName || redactMapKeys {
				childFieldName = ""
			}
			if redactKeys {
				if err := visitConfigYAMLContextNode(
					keyNode,
					"",
					entryOrdinalPath+"/key",
					entrySemanticPath+"/key",
					true,
					seen,
					visit,
				); err != nil {
					return err
				}
			}
			if err := visitConfigYAMLContextNode(
				valueNode,
				childFieldName,
				entryOrdinalPath+"/value",
				entrySemanticPath+"/value",
				valueSensitive,
				seen,
				visit,
			); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		for index, child := range node.Content {
			childSemanticPath := semanticPath + "/sequence"
			if semanticID := configYAMLSemanticSequenceItemID(child, fieldName, valueSensitive); semanticID != "" {
				childSemanticPath += "@" + semanticID
			}
			if err := visitConfigYAMLContextNode(
				child,
				fieldName,
				fmt.Sprintf("%s/sequence%d", ordinalPath, index),
				childSemanticPath,
				valueSensitive,
				seen,
				visit,
			); err != nil {
				return err
			}
		}
	case yaml.AliasNode:
		return visitConfigYAMLContextNode(
			node.Alias,
			fieldName,
			ordinalPath+"/alias",
			semanticPath+"/alias",
			valueSensitive,
			seen,
			visit,
		)
	}
	return nil
}

func configYAMLSemanticMapKey(node *yaml.Node, sensitive bool) string {
	if sensitive {
		return "secret"
	}
	_, value, ok := yamlSemanticScalar(node)
	if !ok {
		return "complex"
	}
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:8])
}

func configYAMLSemanticSequenceItemID(node *yaml.Node, fieldName string, sensitive bool) string {
	if node == nil || node.Kind == yaml.ScalarNode {
		return ""
	}
	signature := configYAMLNodeSignature(
		node,
		fieldName,
		sensitive,
		make(map[*yaml.Node]struct{}),
	)
	digest := sha256.Sum256([]byte(signature))
	return hex.EncodeToString(digest[:8])
}

func configYAMLNodeSignature(
	node *yaml.Node,
	fieldName string,
	inheritedSensitive bool,
	seen map[*yaml.Node]struct{},
) string {
	if node == nil {
		return "nil"
	}
	if _, ok := seen[node]; ok {
		return "shared"
	}
	seen[node] = struct{}{}

	valueSensitive := inheritedSensitive || isValueSensitiveConfigField(fieldName)
	switch node.Kind {
	case yaml.DocumentNode:
		parts := make([]string, 0, len(node.Content))
		for _, child := range node.Content {
			parts = append(parts, configYAMLNodeSignature(child, fieldName, valueSensitive, seen))
		}
		return "document(" + strings.Join(parts, ",") + ")"
	case yaml.MappingNode:
		redactMapKeys := shouldRedactMapKey(fieldName)
		redactKeys := valueSensitive || redactMapKeys
		parts := make([]string, 0, len(node.Content)/2)
		for index := 0; index+1 < len(node.Content); index += 2 {
			keyNode := node.Content[index]
			valueNode := node.Content[index+1]
			_, childFieldName, hasFieldName := yamlSemanticScalar(keyNode)
			if !hasFieldName || redactMapKeys {
				childFieldName = ""
			}
			keySignature := configYAMLSemanticMapKey(keyNode, redactKeys)
			valueSignature := configYAMLNodeSignature(valueNode, childFieldName, valueSensitive, seen)
			parts = append(parts, keySignature+":"+valueSignature)
		}
		sort.Strings(parts)
		return "mapping(" + strings.Join(parts, ",") + ")"
	case yaml.SequenceNode:
		parts := make([]string, 0, len(node.Content))
		for _, child := range node.Content {
			parts = append(parts, configYAMLNodeSignature(child, fieldName, valueSensitive, seen))
		}
		sort.Strings(parts)
		return "sequence(" + strings.Join(parts, ",") + ")"
	case yaml.AliasNode:
		return "alias(" + configYAMLNodeSignature(node.Alias, fieldName, valueSensitive, seen) + ")"
	case yaml.ScalarNode:
		if valueSensitive || isConfigYAMLPlaceholder(node.Value) {
			return "secret"
		}
		return "scalar(" + node.Tag + ":" + node.Value + ")"
	default:
		return fmt.Sprintf("kind(%d)", node.Kind)
	}
}

func isValueSensitiveConfigField(fieldName string) bool {
	return isSensitiveConfigField(fieldName) &&
		!shouldRedactMapKey(fieldName) &&
		!isStructuredSensitiveConfigContainer(fieldName)
}

func isStructuredSensitiveConfigContainer(fieldName string) bool {
	normalized := strings.ToLower(strings.NewReplacer("-", "_", " ", "_").Replace(strings.TrimSpace(fieldName)))
	switch normalized {
	case "gemini_api_key", "interactions_api_key", "claude_api_key", "codex_api_key", "xai_api_key", "vertex_api_key", "api_key_entries":
		return true
	default:
		return false
	}
}

func yamlSemanticScalar(node *yaml.Node) (*yaml.Node, string, bool) {
	seen := make(map[*yaml.Node]struct{})
	for node != nil {
		if _, ok := seen[node]; ok {
			return nil, "", false
		}
		seen[node] = struct{}{}
		switch node.Kind {
		case yaml.AliasNode:
			node = node.Alias
		case yaml.ScalarNode:
			return node, node.Value, true
		default:
			return nil, "", false
		}
	}
	return nil, "", false
}

func configSensitiveValues(cfg *config.Config) []string {
	if cfg == nil {
		return nil
	}
	values := uniqueNonEmptyStrings(sensitiveStringsFromValue(reflect.ValueOf(cfg), ""))
	sort.SliceStable(values, func(i, j int) bool {
		if len(values[i]) != len(values[j]) {
			return len(values[i]) > len(values[j])
		}
		maskedI := util.HideAPIKey(values[i])
		maskedJ := util.HideAPIKey(values[j])
		if maskedI != maskedJ {
			return maskedI < maskedJ
		}
		return values[i] < values[j]
	})
	return values
}

func uniqueNonEmptyStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func sensitiveStringsFromValue(value reflect.Value, fieldName string) []string {
	value = indirectValue(value)
	if !value.IsValid() {
		return nil
	}
	if value.Type() == reflect.TypeOf(yaml.Node{}) && value.CanInterface() {
		node := value.Interface().(yaml.Node)
		return sensitiveStringsFromYAMLNode(&node, fieldName, false, make(map[*yaml.Node]struct{}))
	}
	switch value.Kind() {
	case reflect.String:
		if isSensitiveConfigField(fieldName) {
			return []string{value.String()}
		}
		return nil
	case reflect.Struct:
		var out []string
		valueType := value.Type()
		for i := 0; i < value.NumField(); i++ {
			field := valueType.Field(i)
			if field.PkgPath != "" {
				continue
			}
			out = append(out, sensitiveStringsFromValue(value.Field(i), configFieldName(field))...)
		}
		return out
	case reflect.Slice, reflect.Array:
		var out []string
		for i := 0; i < value.Len(); i++ {
			out = append(out, sensitiveStringsFromValue(value.Index(i), fieldName)...)
		}
		return out
	case reflect.Map:
		var out []string
		for _, key := range value.MapKeys() {
			childFieldName := fieldName
			if key.Kind() == reflect.String {
				keyName := key.String()
				if isSensitiveConfigField(fieldName) {
					out = append(out, keyName)
				}
				childFieldName = keyName
			}
			out = append(out, sensitiveStringsFromValue(value.MapIndex(key), childFieldName)...)
		}
		return out
	default:
		return nil
	}
}

func sensitiveStringsFromYAMLNode(
	node *yaml.Node,
	fieldName string,
	sensitiveContext bool,
	_ map[*yaml.Node]struct{},
) []string {
	var out []string
	_ = visitConfigYAMLContextNode(
		node,
		fieldName,
		"$",
		"$",
		sensitiveContext,
		make(map[configYAMLContextVisitState]struct{}),
		func(secretNode *yaml.Node, _ string, _ string, sensitive bool) error {
			if sensitive {
				out = append(out, secretNode.Value)
			}
			return nil
		},
	)
	return out
}

func configFieldName(field reflect.StructField) string {
	for _, tagName := range []string{"json", "yaml"} {
		if tag := field.Tag.Get(tagName); tag != "" {
			name := strings.TrimSpace(strings.Split(tag, ",")[0])
			if name != "" && name != "-" {
				return name
			}
		}
	}
	return field.Name
}

func isSensitiveConfigField(fieldName string) bool {
	name := strings.ToLower(strings.TrimSpace(fieldName))
	if name == "" {
		return false
	}
	if util.IsSensitiveFieldName(name) {
		return true
	}
	compact := strings.NewReplacer("-", "", "_", "", ".", "").Replace(name)
	return strings.Contains(compact, "apikey") || strings.Contains(compact, "secret") || strings.Contains(compact, "token")
}

func indirectValue(value reflect.Value) reflect.Value {
	for value.IsValid() && (value.Kind() == reflect.Pointer || value.Kind() == reflect.Interface) {
		if value.IsNil() {
			return reflect.Value{}
		}
		value = value.Elem()
	}
	return value
}

func resolveSubmittedAPIKey(value string, candidates []string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return value, nil
	}
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate) == trimmed {
			return trimmed, nil
		}
	}
	if exactMatches := matchingMaskedAPIKeyCandidates(trimmed, candidates); len(exactMatches) > 0 {
		if len(exactMatches) == 1 {
			return exactMatches[0], nil
		}
		return "", fmt.Errorf("ambiguous API key placeholder")
	}
	base, ordinal, hasOrdinal := splitMaskedAPIKeyOrdinal(trimmed)
	if !strings.Contains(base, "...") && base != "***" {
		return trimmed, nil
	}
	matches := matchingMaskedAPIKeyCandidates(base, candidates)
	if len(matches) == 0 {
		if base == "***" || hasOrdinal {
			return "", fmt.Errorf("api key placeholder could not be resolved")
		}
		return trimmed, nil
	}
	if hasOrdinal {
		if ordinal >= 1 && ordinal <= len(matches) {
			return matches[ordinal-1], nil
		}
		return "", fmt.Errorf("api key placeholder could not be resolved")
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	return "", fmt.Errorf("ambiguous API key placeholder")
}

func splitMaskedAPIKeyOrdinal(value string) (string, int, bool) {
	base := strings.TrimSpace(value)
	ordinalValue := strings.TrimRight(base, "#")
	index := strings.LastIndex(ordinalValue, "#")
	if index < 0 {
		return base, 0, false
	}
	ordinal, err := strconv.Atoi(ordinalValue[index+1:])
	if err != nil || ordinal <= 0 {
		return base, 0, false
	}
	return strings.TrimSpace(ordinalValue[:index]), ordinal, true
}

func matchingMaskedAPIKeyCandidates(masked string, candidates []string) []string {
	seen := map[string]struct{}{}
	matches := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if util.HideAPIKey(candidate) != masked {
			continue
		}
		if _, ok := seen[candidate]; ok {
			continue
		}
		seen[candidate] = struct{}{}
		matches = append(matches, candidate)
	}
	return matches
}

func maskedAPIKeyMatches(value string, candidate string) bool {
	candidate = strings.TrimSpace(candidate)
	if candidate == "" {
		return false
	}
	if util.HideAPIKey(candidate) == strings.TrimSpace(value) {
		return true
	}
	base, _, hasOrdinal := splitMaskedAPIKeyOrdinal(value)
	if hasOrdinal {
		return false
	}
	if !strings.Contains(base, "...") && base != "***" {
		return false
	}
	return util.HideAPIKey(candidate) == base
}

func maskAPIKeyList(values []string) []string {
	masks := make([]string, len(values))
	groups := make(map[string]map[string]struct{}, len(values))
	for i, value := range values {
		trimmed := strings.TrimSpace(value)
		mask := util.HideAPIKey(trimmed)
		masks[i] = mask
		if groups[mask] == nil {
			groups[mask] = make(map[string]struct{})
		}
		groups[mask][trimmed] = struct{}{}
	}

	ordinals := make(map[string]map[string]int, len(groups))
	for i, value := range values {
		mask := masks[i]
		if len(groups[mask]) < 2 {
			continue
		}
		trimmed := strings.TrimSpace(value)
		if ordinals[mask] == nil {
			ordinals[mask] = make(map[string]int)
		}
		ordinal := ordinals[mask][trimmed]
		if ordinal == 0 {
			ordinal = len(ordinals[mask]) + 1
			ordinals[mask][trimmed] = ordinal
		}
		masks[i] = mask + "#" + strconv.Itoa(ordinal)
	}
	return masks
}

func resolveStringListPlaceholders(values []string, existing []string) ([]string, error) {
	out := append([]string(nil), values...)
	candidates := append([]string(nil), existing...)
	for i := range out {
		resolved, err := resolveSubmittedAPIKey(out[i], candidates)
		if err != nil {
			return nil, err
		}
		out[i] = resolved
	}
	return out, nil
}

func resolveAliasMapPlaceholders(aliases map[string]string, existing map[string]string) (map[string]string, error) {
	if len(aliases) == 0 {
		return aliases, nil
	}
	candidates := aliasKeyCandidates(existing)
	out := make(map[string]string, len(aliases))
	for key, alias := range aliases {
		resolved, err := resolveSubmittedAPIKey(key, candidates)
		if err != nil {
			return nil, err
		}
		if _, exists := out[resolved]; exists {
			return nil, fmt.Errorf("duplicate API key after placeholder resolution")
		}
		out[resolved] = alias
	}
	return out, nil
}

func aliasKeyCandidates(aliases map[string]string) []string {
	candidates := make([]string, 0, len(aliases))
	for key := range aliases {
		candidates = append(candidates, key)
	}
	sort.Strings(candidates)
	return candidates
}

func resolveStructListAPIKeyPlaceholders[T any](
	values []T,
	existing []T,
	resource structListResource[T],
) ([]T, []*T, error) {
	out := append([]T(nil), values...)
	matchedEntries := make([]*T, len(out))
	usedExisting := make(map[int]struct{}, len(existing))
	candidates := collectSensitiveStringsFromStructList(existing)
	apiKeyCandidates := collectStructuredAPIKeyStringsFromValue(reflect.ValueOf(existing), "")
	for i := range out {
		submittedEntry := out[i]
		hasSubmittedPlaceholders := hasSensitivePlaceholderInValue(
			reflect.ValueOf(submittedEntry),
			candidates,
			apiKeyCandidates,
			"",
		)
		if hasSubmittedPlaceholders {
			rawEntry, err := json.Marshal(submittedEntry)
			if err != nil {
				return nil, nil, fmt.Errorf("marshal submitted entry: %w", err)
			}
			if err = json.Unmarshal(rawEntry, &submittedEntry); err != nil {
				return nil, nil, fmt.Errorf("clone submitted entry: %w", err)
			}
		}

		matchedIndex := findMatchingStructEntryIndex(out[i], existing, resource, usedExisting)
		exactMatch := matchedIndex >= 0 && equalIgnoringSensitiveValues(
			reflect.ValueOf(out[i]),
			reflect.ValueOf(existing[matchedIndex]),
			"",
		)
		if !exactMatch && (matchedIndex < 0 || len(out) != len(existing)) {
			placeholderSources, hasPlaceholders := matchingSensitivePlaceholderEntryIndexes(
				out[i],
				existing,
				candidates,
				apiKeyCandidates,
			)
			if hasPlaceholders && matchedIndex >= 0 {
				if len(placeholderSources) != 1 {
					return nil, nil, fmt.Errorf("masked value has ambiguous entry identity")
				}
				if matchedIndex != placeholderSources[0] {
					return nil, nil, fmt.Errorf("masked value conflicts with entry identity")
				}
			}
		}
		if matchedIndex < 0 && len(out) == len(existing) && i < len(existing) {
			if _, used := usedExisting[i]; !used && equalIgnoringSensitiveValues(
				reflect.ValueOf(out[i]),
				reflect.ValueOf(existing[i]),
				"",
			) {
				matchedIndex = i
			}
		}
		var same reflect.Value
		entryCandidates := candidates
		entryAPIKeyCandidates := apiKeyCandidates
		if matchedIndex >= 0 {
			usedExisting[matchedIndex] = struct{}{}
			matched := existing[matchedIndex]
			matchedEntries[i] = &matched
			same = reflect.ValueOf(matched)
		} else {
			if hasSensitivePlaceholderInValue(
				reflect.ValueOf(out[i]),
				candidates,
				apiKeyCandidates,
				"",
			) {
				return nil, nil, fmt.Errorf("masked value could not be safely resolved for an unmatched entry")
			}
			entryCandidates = nil
			entryAPIKeyCandidates = nil
		}
		if err := resolveSensitiveStringsInValue(
			reflect.ValueOf(&out[i]),
			same,
			entryCandidates,
			entryAPIKeyCandidates,
		); err != nil {
			return nil, nil, err
		}
		if hasSubmittedPlaceholders && matchedIndex >= 0 && !resolvedSensitivePlaceholdersBelongToEntry(
			reflect.ValueOf(submittedEntry),
			reflect.ValueOf(out[i]),
			candidates,
			apiKeyCandidates,
			sensitiveStringsFromValue(same, ""),
			collectStructuredAPIKeyStringsFromValue(same, ""),
			"",
		) {
			return nil, nil, fmt.Errorf("masked value conflicts with entry credentials")
		}
	}
	return out, matchedEntries, nil
}

func resolveStructEntryAPIKeyPlaceholders[T any](value *T, existing T, candidates []string) error {
	if value == nil {
		return nil
	}
	return resolveSensitiveStringsInValue(
		reflect.ValueOf(value),
		reflect.ValueOf(existing),
		candidates,
		collectStructuredAPIKeyStringsFromValue(reflect.ValueOf(existing), ""),
	)
}

func findMatchingStructEntryIndex[T any](
	value T,
	existing []T,
	resource structListResource[T],
	excluded map[int]struct{},
) int {
	uniqueMatch := func(matches []int) int {
		if len(matches) == 1 {
			return matches[0]
		}
		return -1
	}
	available := func(index int) bool {
		_, skip := excluded[index]
		return !skip
	}

	matches := make([]int, 0, 1)
	for i := range existing {
		if available(i) && equalIgnoringSensitiveValues(reflect.ValueOf(value), reflect.ValueOf(existing[i]), "") {
			matches = append(matches, i)
		}
	}
	if matched := uniqueMatch(matches); matched >= 0 {
		return matched
	}

	matchValue := strings.TrimSpace(resource.matchKey(&value))
	if matchValue != "" {
		matches = matches[:0]
		for i := range existing {
			if available(i) && strings.TrimSpace(resource.matchKey(&existing[i])) == matchValue {
				matches = append(matches, i)
			}
		}
		if matched := uniqueMatch(matches); matched >= 0 {
			return matched
		}
	}

	if resource.secondaryMatchKey != nil {
		secondaryValue := strings.TrimSpace(resource.secondaryMatchKey(&value))
		if secondaryValue != "" {
			matches = matches[:0]
			for i := range existing {
				if available(i) && strings.TrimSpace(resource.secondaryMatchKey(&existing[i])) == secondaryValue {
					matches = append(matches, i)
				}
			}
			if matched := uniqueMatch(matches); matched >= 0 {
				return matched
			}
		}
	}
	return -1
}

func equalIgnoringSensitiveValues(left, right reflect.Value, fieldName string) bool {
	left = indirectValue(left)
	right = indirectValue(right)
	if !left.IsValid() || !right.IsValid() {
		return !left.IsValid() && !right.IsValid()
	}
	if left.Type() != right.Type() {
		return false
	}
	if isSensitiveConfigField(fieldName) {
		return true
	}
	switch left.Kind() {
	case reflect.Struct:
		leftType := left.Type()
		for i := 0; i < left.NumField(); i++ {
			field := leftType.Field(i)
			if field.PkgPath != "" {
				continue
			}
			if !equalIgnoringSensitiveValues(left.Field(i), right.Field(i), configFieldName(field)) {
				return false
			}
		}
		return true
	case reflect.Slice, reflect.Array:
		if left.Len() != right.Len() {
			return false
		}
		for i := 0; i < left.Len(); i++ {
			if !equalIgnoringSensitiveValues(left.Index(i), right.Index(i), fieldName) {
				return false
			}
		}
		return true
	case reflect.Map:
		if left.Len() != right.Len() {
			return false
		}
		for _, key := range left.MapKeys() {
			rightValue := right.MapIndex(key)
			if !rightValue.IsValid() {
				return false
			}
			childFieldName := fieldName
			if key.Kind() == reflect.String {
				childFieldName = key.String()
			}
			if !equalIgnoringSensitiveValues(left.MapIndex(key), rightValue, childFieldName) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(left.Interface(), right.Interface())
	}
}

func collectSensitiveStringsFromStructList[T any](values []T) []string {
	var out []string
	for i := range values {
		out = append(out, sensitiveStringsFromValue(reflect.ValueOf(values[i]), "")...)
	}
	return uniqueNonEmptyStrings(out)
}

func collectStructuredAPIKeyStringsFromValue(value reflect.Value, fieldName string) []string {
	value = indirectValue(value)
	if !value.IsValid() {
		return nil
	}
	switch value.Kind() {
	case reflect.String:
		if isStructuredAPIKeyConfigField(fieldName) {
			return uniqueNonEmptyStrings([]string{value.String()})
		}
	case reflect.Struct:
		var out []string
		valueType := value.Type()
		for i := 0; i < value.NumField(); i++ {
			field := valueType.Field(i)
			if field.PkgPath != "" {
				continue
			}
			out = append(
				out,
				collectStructuredAPIKeyStringsFromValue(value.Field(i), configFieldName(field))...,
			)
		}
		return uniqueNonEmptyStrings(out)
	case reflect.Slice, reflect.Array:
		var out []string
		for i := 0; i < value.Len(); i++ {
			out = append(out, collectStructuredAPIKeyStringsFromValue(value.Index(i), fieldName)...)
		}
		return uniqueNonEmptyStrings(out)
	}
	return nil
}

func isStructuredAPIKeyConfigField(fieldName string) bool {
	normalized := strings.ToLower(strings.NewReplacer("-", "_", " ", "_").Replace(strings.TrimSpace(fieldName)))
	return normalized == "api_key" || normalized == "apikey"
}

func matchingSensitivePlaceholderEntryIndexes[T any](
	value T,
	existing []T,
	candidates []string,
	apiKeyCandidates []string,
) ([]int, bool) {
	valueRef := reflect.ValueOf(value)
	if !hasSensitivePlaceholderInValue(valueRef, candidates, apiKeyCandidates, "") {
		return nil, false
	}

	matches := make([]int, 0, 1)
	for i := range existing {
		existingRef := reflect.ValueOf(existing[i])
		entryCandidates := sensitiveStringsFromValue(existingRef, "")
		entryAPIKeyCandidates := collectStructuredAPIKeyStringsFromValue(existingRef, "")
		if sensitivePlaceholdersResolveAgainstCandidates(
			valueRef,
			candidates,
			apiKeyCandidates,
			entryCandidates,
			entryAPIKeyCandidates,
			"",
		) {
			matches = append(matches, i)
		}
	}
	return matches, true
}

func sensitivePlaceholdersResolveAgainstCandidates(
	value reflect.Value,
	candidates []string,
	apiKeyCandidates []string,
	entryCandidates []string,
	entryAPIKeyCandidates []string,
	fieldName string,
) bool {
	value = indirectValue(value)
	if !value.IsValid() {
		return true
	}
	switch value.Kind() {
	case reflect.String:
		if !isSensitiveConfigField(fieldName) {
			return true
		}
		globalCandidates := candidates
		localCandidates := entryCandidates
		if isStructuredAPIKeyConfigField(fieldName) {
			globalCandidates = apiKeyCandidates
			localCandidates = entryAPIKeyCandidates
		}
		trimmed := strings.TrimSpace(value.String())
		if !isSensitivePlaceholderString(trimmed, fieldName, globalCandidates) {
			return true
		}
		return isSensitivePlaceholderString(trimmed, fieldName, localCandidates)
	case reflect.Struct:
		valueType := value.Type()
		for i := 0; i < value.NumField(); i++ {
			field := valueType.Field(i)
			if field.PkgPath != "" {
				continue
			}
			if !sensitivePlaceholdersResolveAgainstCandidates(
				value.Field(i),
				candidates,
				apiKeyCandidates,
				entryCandidates,
				entryAPIKeyCandidates,
				configFieldName(field),
			) {
				return false
			}
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < value.Len(); i++ {
			if !sensitivePlaceholdersResolveAgainstCandidates(
				value.Index(i),
				candidates,
				apiKeyCandidates,
				entryCandidates,
				entryAPIKeyCandidates,
				fieldName,
			) {
				return false
			}
		}
	case reflect.Map:
		if value.Type().Key().Kind() != reflect.String || value.Type().Elem().Kind() != reflect.String {
			return true
		}
		for _, key := range value.MapKeys() {
			if !sensitivePlaceholdersResolveAgainstCandidates(
				value.MapIndex(key),
				candidates,
				apiKeyCandidates,
				entryCandidates,
				entryAPIKeyCandidates,
				key.String(),
			) {
				return false
			}
		}
	}
	return true
}

func resolvedSensitivePlaceholdersBelongToEntry(
	submitted reflect.Value,
	resolved reflect.Value,
	candidates []string,
	apiKeyCandidates []string,
	entryCandidates []string,
	entryAPIKeyCandidates []string,
	fieldName string,
) bool {
	submitted = indirectValue(submitted)
	resolved = indirectValue(resolved)
	if !submitted.IsValid() || !resolved.IsValid() || submitted.Type() != resolved.Type() {
		return false
	}
	switch submitted.Kind() {
	case reflect.String:
		if !isSensitiveConfigField(fieldName) {
			return true
		}
		globalCandidates := candidates
		localCandidates := entryCandidates
		if isStructuredAPIKeyConfigField(fieldName) {
			globalCandidates = apiKeyCandidates
			localCandidates = entryAPIKeyCandidates
		}
		if !isSensitivePlaceholderString(strings.TrimSpace(submitted.String()), fieldName, globalCandidates) {
			return true
		}
		resolvedValue := strings.TrimSpace(resolved.String())
		for _, candidate := range localCandidates {
			if strings.TrimSpace(candidate) == resolvedValue {
				return true
			}
		}
		return false
	case reflect.Struct:
		valueType := submitted.Type()
		for i := 0; i < submitted.NumField(); i++ {
			field := valueType.Field(i)
			if field.PkgPath != "" {
				continue
			}
			if !resolvedSensitivePlaceholdersBelongToEntry(
				submitted.Field(i),
				resolved.Field(i),
				candidates,
				apiKeyCandidates,
				entryCandidates,
				entryAPIKeyCandidates,
				configFieldName(field),
			) {
				return false
			}
		}
	case reflect.Slice, reflect.Array:
		if submitted.Len() != resolved.Len() {
			return false
		}
		for i := 0; i < submitted.Len(); i++ {
			if !resolvedSensitivePlaceholdersBelongToEntry(
				submitted.Index(i),
				resolved.Index(i),
				candidates,
				apiKeyCandidates,
				entryCandidates,
				entryAPIKeyCandidates,
				fieldName,
			) {
				return false
			}
		}
	case reflect.Map:
		if submitted.Type().Key().Kind() != reflect.String || submitted.Type().Elem().Kind() != reflect.String {
			return true
		}
		for _, key := range submitted.MapKeys() {
			resolvedValue := resolved.MapIndex(key)
			if !resolvedValue.IsValid() || !resolvedSensitivePlaceholdersBelongToEntry(
				submitted.MapIndex(key),
				resolvedValue,
				candidates,
				apiKeyCandidates,
				entryCandidates,
				entryAPIKeyCandidates,
				key.String(),
			) {
				return false
			}
		}
	}
	return true
}

func isSensitivePlaceholderString(value, fieldName string, candidates []string) bool {
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate) == value {
			return false
		}
	}
	if len(matchingSensitiveConfigCandidates(value, fieldName, candidates)) > 0 {
		return true
	}
	base, ordinal, hasOrdinal := splitMaskedAPIKeyOrdinal(value)
	if !hasOrdinal {
		return false
	}
	matches := matchingSensitiveConfigCandidates(base, fieldName, candidates)
	return ordinal >= 1 && ordinal <= len(matches)
}

func hasSensitivePlaceholderInValue(
	value reflect.Value,
	candidates []string,
	apiKeyCandidates []string,
	fieldName string,
) bool {
	value = indirectValue(value)
	if !value.IsValid() {
		return false
	}
	switch value.Kind() {
	case reflect.String:
		if !isSensitiveConfigField(fieldName) {
			return false
		}
		fieldCandidates := candidates
		if isStructuredAPIKeyConfigField(fieldName) {
			fieldCandidates = apiKeyCandidates
		}
		trimmed := strings.TrimSpace(value.String())
		for _, candidate := range fieldCandidates {
			if strings.TrimSpace(candidate) == trimmed {
				return false
			}
		}
		if len(matchingSensitiveConfigCandidates(trimmed, fieldName, fieldCandidates)) > 0 {
			return true
		}
		base, _, hasOrdinal := splitMaskedAPIKeyOrdinal(trimmed)
		return hasOrdinal && len(matchingSensitiveConfigCandidates(base, fieldName, fieldCandidates)) > 0
	case reflect.Struct:
		valueType := value.Type()
		for i := 0; i < value.NumField(); i++ {
			field := valueType.Field(i)
			if field.PkgPath == "" && hasSensitivePlaceholderInValue(
				value.Field(i),
				candidates,
				apiKeyCandidates,
				configFieldName(field),
			) {
				return true
			}
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < value.Len(); i++ {
			if hasSensitivePlaceholderInValue(value.Index(i), candidates, apiKeyCandidates, fieldName) {
				return true
			}
		}
	case reflect.Map:
		if value.Type().Key().Kind() != reflect.String || value.Type().Elem().Kind() != reflect.String {
			return false
		}
		for _, key := range value.MapKeys() {
			if hasSensitivePlaceholderInValue(value.MapIndex(key), candidates, apiKeyCandidates, key.String()) {
				return true
			}
		}
	}
	return false
}

func resolveSensitiveStringsInValue(
	value reflect.Value,
	same reflect.Value,
	candidates []string,
	apiKeyCandidates []string,
) error {
	return resolveSensitiveStringsInValueForField(value, same, candidates, apiKeyCandidates, "")
}

func resolveSensitiveStringsInValueForField(
	value reflect.Value,
	same reflect.Value,
	candidates []string,
	apiKeyCandidates []string,
	containerFieldName string,
) error {
	value = indirectValue(value)
	same = indirectValue(same)
	if !value.IsValid() {
		return nil
	}
	switch value.Kind() {
	case reflect.Struct:
		valueType := value.Type()
		for i := 0; i < value.NumField(); i++ {
			field := valueType.Field(i)
			if field.PkgPath != "" {
				continue
			}
			fieldValue := value.Field(i)
			var sameField reflect.Value
			if same.IsValid() && same.Kind() == reflect.Struct && i < same.NumField() {
				sameField = same.Field(i)
			}
			fieldName := configFieldName(field)
			fieldCandidates := candidates
			if isStructuredAPIKeyConfigField(fieldName) {
				fieldCandidates = apiKeyCandidates
			}
			if isSensitiveConfigField(fieldName) && fieldValue.Kind() == reflect.String && fieldValue.CanSet() {
				resolved, err := resolveSensitiveConfigString(
					fieldValue.String(),
					fieldName,
					reflectStringValue(sameField),
					fieldCandidates,
				)
				if err != nil {
					return err
				}
				fieldValue.SetString(resolved)
				continue
			}
			childAPIKeyCandidates := apiKeyCandidates
			if isStructuredSensitiveConfigContainer(fieldName) {
				if localCandidates := collectStructuredAPIKeyStringsFromValue(sameField, fieldName); len(localCandidates) > 0 {
					childAPIKeyCandidates = localCandidates
				}
			}
			if err := resolveSensitiveStringsInValueForField(
				fieldValue,
				sameField,
				candidates,
				childAPIKeyCandidates,
				fieldName,
			); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < value.Len(); i++ {
			sameItem := findMatchingValue(value.Index(i), same)
			if !sameItem.IsValid() && same.IsValid() &&
				(same.Kind() == reflect.Slice || same.Kind() == reflect.Array) &&
				value.Len() == same.Len() && i < same.Len() &&
				equalIgnoringSensitiveValues(value.Index(i), same.Index(i), "") {
				sameItem = same.Index(i)
			}
			if err := resolveSensitiveStringsInValueForField(
				value.Index(i),
				sameItem,
				candidates,
				apiKeyCandidates,
				containerFieldName,
			); err != nil {
				return err
			}
		}
	case reflect.Map:
		if value.Type().Key().Kind() != reflect.String || value.Type().Elem().Kind() != reflect.String {
			return nil
		}
		for _, key := range value.MapKeys() {
			fieldName := key.String()
			if !isSensitiveConfigField(fieldName) {
				continue
			}
			mapValue := value.MapIndex(key)
			var sameValue string
			if same.IsValid() && same.Kind() == reflect.Map {
				sameValue = reflectStringValue(same.MapIndex(key))
			}
			resolved, err := resolveSensitiveConfigString(mapValue.String(), fieldName, sameValue, candidates)
			if err != nil {
				return err
			}
			value.SetMapIndex(key, reflect.ValueOf(resolved).Convert(value.Type().Elem()))
		}
	}
	return nil
}

func findMatchingValue(value reflect.Value, candidates reflect.Value) reflect.Value {
	candidates = indirectValue(candidates)
	if !candidates.IsValid() || (candidates.Kind() != reflect.Slice && candidates.Kind() != reflect.Array) {
		return reflect.Value{}
	}
	matchedIndex := -1
	for i := 0; i < candidates.Len(); i++ {
		if !equalIgnoringSensitiveValues(value, candidates.Index(i), "") {
			continue
		}
		if matchedIndex >= 0 {
			return reflect.Value{}
		}
		matchedIndex = i
	}
	if matchedIndex < 0 {
		return reflect.Value{}
	}
	return candidates.Index(matchedIndex)
}

func reflectStringValue(value reflect.Value) string {
	value = indirectValue(value)
	if !value.IsValid() || value.Kind() != reflect.String {
		return ""
	}
	return value.String()
}

func resolveSensitiveConfigString(value, fieldName, same string, candidates []string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return value, nil
	}
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate) == trimmed {
			return trimmed, nil
		}
	}
	if same = strings.TrimSpace(same); same != "" && maskSensitiveConfigString(fieldName, same) == trimmed {
		return same, nil
	}
	if exactMatches := matchingSensitiveConfigCandidates(trimmed, fieldName, candidates); len(exactMatches) > 0 {
		if len(exactMatches) == 1 {
			return exactMatches[0], nil
		}
		return "", fmt.Errorf("ambiguous masked value for %s", fieldName)
	}

	base, ordinal, hasOrdinal := splitMaskedAPIKeyOrdinal(trimmed)
	maskedValue := trimmed
	if hasOrdinal {
		maskedValue = base
	}
	if same = strings.TrimSpace(same); same != "" && maskSensitiveConfigString(fieldName, same) == maskedValue && !hasOrdinal {
		return same, nil
	}

	matches := matchingSensitiveConfigCandidates(maskedValue, fieldName, candidates)
	if hasOrdinal {
		if ordinal >= 1 && ordinal <= len(matches) {
			return matches[ordinal-1], nil
		}
		return "", fmt.Errorf("masked value for %s could not be resolved", fieldName)
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("ambiguous masked value for %s", fieldName)
	}
	return value, nil
}

func matchingSensitiveConfigCandidates(maskedValue, fieldName string, candidates []string) []string {
	matches := make([]string, 0, 1)
	seen := make(map[string]struct{})
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" || maskSensitiveConfigString(fieldName, candidate) != maskedValue {
			continue
		}
		if _, ok := seen[candidate]; ok {
			continue
		}
		seen[candidate] = struct{}{}
		matches = append(matches, candidate)
	}
	return matches
}

func maskSensitiveConfigString(fieldName, value string) string {
	if strings.Contains(strings.ToLower(strings.TrimSpace(fieldName)), "authorization") {
		return util.MaskSensitiveHeaderValue(fieldName, value)
	}
	return util.HideAPIKey(value)
}

func resolveStructMatchPlaceholder[T any](
	list []T,
	r structListResource[T],
	matchValue string,
	secondaryValue string,
	hasSecondary bool,
) (string, error) {
	candidates := make([]string, 0, len(list))
	for i := range list {
		candidates = append(candidates, r.matchKey(&list[i]))
	}
	_, _, hasOrdinal := splitMaskedAPIKeyOrdinal(strings.TrimSpace(matchValue))
	if hasOrdinal || !hasSecondary || r.secondaryMatchKey == nil {
		return resolveSubmittedAPIKey(matchValue, candidates)
	}

	secondaryCandidates := make([]string, 0, len(list))
	for i := range list {
		if r.secondaryMatchKey(&list[i]) == secondaryValue {
			secondaryCandidates = append(secondaryCandidates, r.matchKey(&list[i]))
		}
	}
	resolved, err := resolveSubmittedAPIKey(matchValue, secondaryCandidates)
	if err == nil {
		return resolved, nil
	}
	return resolveSubmittedAPIKey(matchValue, candidates)
}
