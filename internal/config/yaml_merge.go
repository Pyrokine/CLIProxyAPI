package config

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// NormalizeCommentIndentation aligns standalone comments when the YAML tree stays unchanged.
func NormalizeCommentIndentation(data []byte) []byte {
	lines := bytes.Split(data, []byte("\n"))
	changed := false
	for i, line := range lines {
		trimmed := bytes.TrimLeft(line, " \t")
		if len(trimmed) > 0 && trimmed[0] == '#' && len(trimmed) < len(line) {
			lines[i] = trimmed
			changed = true
		}
	}
	if !changed {
		return data
	}
	// A hash at the start of an indented line can be part of a scalar value.
	candidate := bytes.Join(lines, []byte("\n"))
	original := yaml.NewDecoder(bytes.NewReader(data))
	normalized := yaml.NewDecoder(bytes.NewReader(candidate))
	for {
		var before, after yaml.Node
		beforeErr, afterErr := original.Decode(&before), normalized.Decode(&after)
		if beforeErr == io.EOF && afterErr == io.EOF {
			return candidate
		}
		if beforeErr != nil || afterErr != nil {
			return data
		}
		clearYAMLPositions(&before)
		clearYAMLPositions(&after)
		if !reflect.DeepEqual(before, after) {
			return data
		}
	}
}

func clearYAMLPositions(node *yaml.Node) {
	node.Line, node.Column = 0, 0
	for _, child := range node.Content {
		clearYAMLPositions(child)
	}
}

// mergeMappingPreserve merges src into dst, preserving existing key order and comments.
func mergeMappingPreserve(dst, src *yaml.Node, includeDefaults bool, path ...[]string) error {
	var currentPath []string
	if len(path) > 0 {
		currentPath = path[0]
	}

	if dst == nil || src == nil {
		return nil
	}
	if dst.Kind != yaml.MappingNode || src.Kind != yaml.MappingNode {
		copyNodeShallow(dst, src)
		return nil
	}
	hasMerge := yamlMappingValue(dst, "<<") != nil
	for i := 0; i+1 < len(src.Content); i += 2 {
		sk := src.Content[i]
		sv := src.Content[i+1]
		if sk.Value == "disable-private-remote-ips" && slices.Equal(
			currentPath, []string{"codex", "live-media-relay"},
		) {
			if legacy := findMapKeyIndex(dst, "allow-private-remote-ips"); legacy >= 0 {
				// Keep the source spelling and polarity; loading both spellings is invalid.
				sk = dst.Content[legacy]
				sv = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: strconv.FormatBool(sv.Value == "false")}
			}
		}
		idx := findMapKeyIndex(dst, sk.Value)
		childPath := appendPath(currentPath, sk.Value)
		if idx >= 0 {
			if err := mergeNodePreserve(dst.Content[idx+1], sv, includeDefaults, childPath); err != nil {
				return err
			}
			continue
		}
		var inherited *yaml.Node
		if hasMerge && (sv.Kind == yaml.ScalarNode || sv.Kind == yaml.SequenceNode) {
			var err error
			inherited, _, err = configYAMLEffectiveMappingValue(dst, sk.Value, nil)
			if err != nil {
				return err
			}
			if inherited != nil {
				var previous, next any
				if err := errors.Join(inherited.Decode(&previous), sv.Decode(&next)); err != nil {
					return err
				}
				if reflect.DeepEqual(previous, next) {
					continue
				}
			}
		}
		candidate := deepCopyNode(sv)
		// A change from an inherited value must also persist when the new value is zero.
		if !includeDefaults && inherited == nil {
			pruneKnownDefaultsInNewNode(childPath, candidate)
			if isKnownDefaultValue(childPath, candidate) {
				continue
			}
		}
		dst.Content = append(dst.Content, deepCopyNode(sk), candidate)
	}
	return nil
}

// mergeNodePreserve merges src into dst for scalars, mappings and sequences while
// reusing destination nodes to keep comments and anchors. For sequences, it updates
// in-place by index.
func mergeNodePreserve(dst, src *yaml.Node, includeDefaults bool, path ...[]string) error {
	var currentPath []string
	if len(path) > 0 {
		currentPath = path[0]
	}

	if dst == nil || src == nil {
		return nil
	}
	if dst.Kind == yaml.AliasNode && dst.Alias != nil {
		resolved := deepCopyNode(dst.Alias)
		resolved.Anchor = ""
		*dst = *resolved
	}
	switch src.Kind {
	case yaml.MappingNode:
		if dst.Kind != yaml.MappingNode {
			copyNodeShallow(dst, src)
		}
		if err := mergeMappingPreserve(dst, src, includeDefaults, currentPath); err != nil {
			return err
		}
		if len(currentPath) > 0 && currentPath[len(currentPath)-1] == "headers" ||
			len(currentPath) == 2 && currentPath[0] == "claude-api-key" && currentPath[1] == "cloak" ||
			len(currentPath) == 3 && currentPath[0] == "api-key-rules" && currentPath[2] == "model-mappings" {
			pruneMissingMapKeys(dst, src)
		}
		if len(currentPath) == 2 && currentPath[0] == "api-key-rules" {
			return pruneMissingConfigMapKeys(dst, src, currentPath)
		}
	case yaml.SequenceNode:
		// Preserve an explicit null when the typed sequence is empty.
		if dst.Kind == yaml.ScalarNode && dst.Tag == "!!null" && len(src.Content) == 0 {
			return nil
		}
		if dst.Kind != yaml.SequenceNode {
			dst.Kind = yaml.SequenceNode
			dst.Tag = "!!seq"
			dst.Content = nil
		}
		type inactiveRow struct {
			index int
			node  *yaml.Node
		}
		var inactiveRows []inactiveRow
		if inactive := inactiveConfigYAMLRow(currentPath); inactive != nil && !slices.ContainsFunc(
			src.Content, inactive,
		) {
			// Rows ignored by the loader remain editable in the source file.
			active := dst.Content[:0]
			for index, row := range dst.Content {
				if inactive(row) {
					inactiveRows = append(inactiveRows, inactiveRow{index, row})
				} else {
					active = append(active, row)
				}
			}
			dst.Content = active
		}
		reorderSequenceForMerge(dst, src)
		// Update elements in place
		minContent := min(len(src.Content), len(dst.Content))
		for i := range minContent {
			if dst.Content[i] == nil {
				dst.Content[i] = deepCopyNode(src.Content[i])
				continue
			}
			if err := mergeNodePreserve(dst.Content[i], src.Content[i], includeDefaults, currentPath); err != nil {
				return err
			}
			if dst.Content[i] != nil && src.Content[i] != nil &&
				dst.Content[i].Kind == yaml.MappingNode && src.Content[i].Kind == yaml.MappingNode {
				if err := pruneMissingConfigMapKeys(dst.Content[i], src.Content[i], currentPath); err != nil {
					return err
				}
			}
		}
		// Append any extra items from src
		for i := len(dst.Content); i < len(src.Content); i++ {
			dst.Content = append(dst.Content, deepCopyNode(src.Content[i]))
		}
		// Truncate if dst has extra items not in src
		if len(src.Content) < len(dst.Content) {
			dst.Content = dst.Content[:len(src.Content)]
		}
		for _, row := range inactiveRows {
			dst.Content = slices.Insert(dst.Content, min(row.index, len(dst.Content)), row.node)
		}
	case yaml.ScalarNode, yaml.AliasNode:
		// For scalars, update Tag and Value but keep Style from dst to preserve quoting
		dst.Kind = src.Kind
		dst.Tag = src.Tag
		dst.Value = src.Value
		// Keep dst.Style as-is intentionally
	case 0:
		// Unknown/empty kind; do nothing
	default:
		copyNodeShallow(dst, src)
	}
	return nil
}

func inactiveConfigYAMLRow(path []string) func(*yaml.Node) bool {
	if len(path) == 1 && path[0] == "codex-api-key" {
		return isInactiveCodexYAMLRow
	}
	if len(path) != 2 {
		return nil
	}
	if path[0] == "payload" && (path[1] == "default-raw" || path[1] == "override-raw") {
		return func(node *yaml.Node) bool {
			var row PayloadRule
			if node.Decode(&row) != nil {
				return false
			}
			_, invalid := InvalidRawPayloadParam(row)
			return invalid
		}
	}
	if path[0] == "plugins" && path[1] == "store-auth" {
		return func(node *yaml.Node) bool {
			if node.Kind != yaml.MappingNode && node.Kind != yaml.AliasNode {
				return false
			}
			var row struct {
				Match string `yaml:"match"`
			}
			return node.Decode(&row) == nil && strings.TrimSpace(row.Match) == ""
		}
	}
	return nil
}

func isInactiveCodexYAMLRow(node *yaml.Node) bool {
	if node.Kind != yaml.MappingNode && node.Kind != yaml.AliasNode {
		return false
	}
	var row struct {
		BaseURL string `yaml:"base-url"`
	}
	return node.Decode(&row) == nil && strings.TrimSpace(row.BaseURL) == ""
}

func reorderSequenceForMerge(dst, src *yaml.Node) {
	if dst == nil || src == nil {
		return
	}
	if len(dst.Content) == 0 {
		return
	}
	if len(src.Content) == 0 {
		return
	}
	original := append([]*yaml.Node(nil), dst.Content...)
	used := make([]bool, len(original))
	ordered := make([]*yaml.Node, len(src.Content))
	for i := range src.Content {
		if idx := matchSequenceElement(original, used, src.Content[i]); idx >= 0 {
			ordered[i] = original[idx]
			used[idx] = true
		}
	}
	dst.Content = ordered
}

func matchSequenceElement(original []*yaml.Node, used []bool, target *yaml.Node) int {
	if target == nil {
		return -1
	}
	switch target.Kind {
	case yaml.MappingNode:
		id := sequenceElementIdentity(target)
		if id != "" {
			for i := range original {
				if used[i] || original[i] == nil || original[i].Kind != yaml.MappingNode {
					continue
				}
				if sequenceElementIdentity(original[i]) == id {
					return i
				}
			}
		}
	case yaml.ScalarNode:
		val := strings.TrimSpace(target.Value)
		if val != "" {
			for i := range original {
				if used[i] || original[i] == nil || original[i].Kind != yaml.ScalarNode {
					continue
				}
				if strings.TrimSpace(original[i].Value) == val {
					return i
				}
			}
		}
	default:
	}
	// Fallback to structural equality to preserve nodes lacking explicit identifiers.
	for i := range original {
		if used[i] || original[i] == nil {
			continue
		}
		if nodesStructurallyEqual(original[i], target) {
			return i
		}
	}
	return -1
}

func sequenceElementIdentity(node *yaml.Node) string {
	if node == nil || node.Kind != yaml.MappingNode {
		return ""
	}
	identityKeys := []string{"id", "name", "alias", "api-key", "api_key", "apikey", "key", "provider", "model"}
	for _, k := range identityKeys {
		if v := mappingScalarValue(node, k); v != "" {
			return k + "=" + v
		}
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode := node.Content[i]
		valNode := node.Content[i+1]
		if keyNode == nil || valNode == nil || valNode.Kind != yaml.ScalarNode {
			continue
		}
		val := strings.TrimSpace(valNode.Value)
		if val != "" {
			return strings.ToLower(strings.TrimSpace(keyNode.Value)) + "=" + val
		}
	}
	return ""
}

func nodesStructurallyEqual(a, b *yaml.Node) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Kind != b.Kind {
		return false
	}
	switch a.Kind {
	case yaml.MappingNode:
		if len(a.Content) != len(b.Content) {
			return false
		}
		for i := 0; i+1 < len(a.Content); i += 2 {
			if !nodesStructurallyEqual(a.Content[i], b.Content[i]) {
				return false
			}
			if !nodesStructurallyEqual(a.Content[i+1], b.Content[i+1]) {
				return false
			}
		}
		return true
	case yaml.SequenceNode:
		if len(a.Content) != len(b.Content) {
			return false
		}
		for i := range a.Content {
			if !nodesStructurallyEqual(a.Content[i], b.Content[i]) {
				return false
			}
		}
		return true
	case yaml.ScalarNode:
		return strings.TrimSpace(a.Value) == strings.TrimSpace(b.Value)
	case yaml.AliasNode:
		return nodesStructurallyEqual(a.Alias, b.Alias)
	default:
		return strings.TrimSpace(a.Value) == strings.TrimSpace(b.Value)
	}
}

// isZeroValueNode returns true if the YAML node represents a zero/default value
// that should not be written as a new key to preserve config cleanliness.
// For mappings and sequences, recursively checks if all children are zero values.
func isZeroValueNode(node *yaml.Node) bool {
	if node == nil {
		return true
	}
	switch node.Kind {
	case yaml.ScalarNode:
		switch node.Tag {
		case "!!bool":
			return node.Value == "false"
		case "!!int", "!!float":
			return node.Value == "0" || node.Value == "0.0"
		case "!!str":
			return node.Value == ""
		case "!!null":
			return true
		}
	case yaml.SequenceNode:
		if len(node.Content) == 0 {
			return true
		}
		// Check if all elements are zero values
		for _, child := range node.Content {
			if !isZeroValueNode(child) {
				return false
			}
		}
		return true
	case yaml.MappingNode:
		if len(node.Content) == 0 {
			return true
		}
		// Check if all values are zero values (values are at odd indices)
		for i := 1; i < len(node.Content); i += 2 {
			if !isZeroValueNode(node.Content[i]) {
				return false
			}
		}
		return true
	default:
	}
	return false
}

// deepCopyNode creates a deep copy of a yaml.Node graph.
func deepCopyNode(n *yaml.Node) *yaml.Node {
	copies := make(map[*yaml.Node]*yaml.Node)
	var clone func(*yaml.Node) *yaml.Node
	clone = func(source *yaml.Node) *yaml.Node {
		if source == nil {
			return nil
		}
		if copied := copies[source]; copied != nil {
			return copied
		}
		copied := *source
		copies[source] = &copied
		if source.Content != nil {
			copied.Content = make([]*yaml.Node, len(source.Content))
			for index, child := range source.Content {
				copied.Content[index] = clone(child)
			}
		}
		copied.Alias = clone(source.Alias)
		return &copied
	}
	return clone(n)
}

// copyNodeShallow copies type/tag/value and resets content to match src, but
// keeps the same destination node pointer to preserve parent relations/comments.
func copyNodeShallow(dst, src *yaml.Node) {
	if dst == nil || src == nil {
		return
	}
	copied := deepCopyNode(src)
	dst.Kind = copied.Kind
	dst.Tag = copied.Tag
	dst.Value = copied.Value
	// 同次复制保留子节点之间的锚点引用
	dst.Content = copied.Content
	dst.Alias = copied.Alias
}

// findMapKeyIndex returns the index of key node in dst mapping (index of key, not value).
// Returns -1 when not found.
func findMapKeyIndex(mapNode *yaml.Node, key string) int {
	if mapNode == nil || mapNode.Kind != yaml.MappingNode {
		return -1
	}
	for i := 0; i+1 < len(mapNode.Content); i += 2 {
		if mapNode.Content[i] != nil && mapNode.Content[i].Value == key {
			return i
		}
	}
	return -1
}

func removeMapKey(mapNode *yaml.Node, key string) {
	if mapNode == nil || mapNode.Kind != yaml.MappingNode || key == "" {
		return
	}
	for i := 0; i+1 < len(mapNode.Content); i += 2 {
		if mapNode.Content[i] != nil && mapNode.Content[i].Value == key {
			mapNode.Content = append(mapNode.Content[:i], mapNode.Content[i+2:]...)
			return
		}
	}
}

// getOrCreateMapValue finds the value node for a given key in a mapping node.
// If not found, it appends a new key/value pair and returns the new value node.
func getOrCreateMapValue(mapNode *yaml.Node, key string) *yaml.Node {
	if mapNode.Kind == yaml.AliasNode && mapNode.Alias != nil {
		resolved := deepCopyNode(mapNode.Alias)
		resolved.Anchor = ""
		*mapNode = *resolved
	}
	if mapNode.Kind != yaml.MappingNode {
		mapNode.Kind = yaml.MappingNode
		mapNode.Tag = "!!map"
		mapNode.Content = nil
	}
	for i := 0; i+1 < len(mapNode.Content); i += 2 {
		k := mapNode.Content[i]
		if k.Value == key {
			return mapNode.Content[i+1]
		}
	}
	// append new key/value
	mapNode.Content = append(mapNode.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key})
	val := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: ""}
	mapNode.Content = append(mapNode.Content, val)
	return val
}

func pruneMappingToGeneratedKeys(dstRoot, srcRoot *yaml.Node, key string) {
	if key == "" || dstRoot == nil || srcRoot == nil {
		return
	}
	if dstRoot.Kind != yaml.MappingNode || srcRoot.Kind != yaml.MappingNode {
		return
	}
	dstIdx := findMapKeyIndex(dstRoot, key)
	if dstIdx < 0 || dstIdx+1 >= len(dstRoot.Content) {
		return
	}
	srcIdx := findMapKeyIndex(srcRoot, key)
	if srcIdx < 0 {
		// Keep explicit empty mappings when the last OAuth channel is deleted through the management API.
		if key == "oauth-model-alias" || key == "oauth-request-scoped-errors" {
			dstRoot.Content[dstIdx+1] = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			return
		}
		removeMapKey(dstRoot, key)
		return
	}
	if srcIdx+1 >= len(srcRoot.Content) {
		return
	}
	srcVal := srcRoot.Content[srcIdx+1]
	dstVal := dstRoot.Content[dstIdx+1]
	if srcVal == nil {
		dstRoot.Content[dstIdx+1] = nil
		return
	}
	if srcVal.Kind != yaml.MappingNode {
		dstRoot.Content[dstIdx+1] = deepCopyNode(srcVal)
		return
	}
	if dstVal == nil || dstVal.Kind != yaml.MappingNode {
		dstRoot.Content[dstIdx+1] = deepCopyNode(srcVal)
		return
	}
	pruneMissingMapKeys(dstVal, srcVal)
}

func prunePluginConfigMappings(dstRoot, srcRoot *yaml.Node) {
	if dstRoot == nil || srcRoot == nil || dstRoot.Kind != yaml.MappingNode ||
		srcRoot.Kind != yaml.MappingNode {
		return
	}
	dstPluginIndex := findMapKeyIndex(dstRoot, "plugins")
	srcPluginIndex := findMapKeyIndex(srcRoot, "plugins")
	if dstPluginIndex < 0 || srcPluginIndex < 0 {
		return
	}
	dstPlugins := dstRoot.Content[dstPluginIndex+1]
	srcPlugins := srcRoot.Content[srcPluginIndex+1]
	pruneMappingToGeneratedKeys(dstPlugins, srcPlugins, "configs")
	if dstPlugins.Kind != yaml.MappingNode || srcPlugins.Kind != yaml.MappingNode {
		return
	}
	dstConfigIndex := findMapKeyIndex(dstPlugins, "configs")
	srcConfigIndex := findMapKeyIndex(srcPlugins, "configs")
	if dstConfigIndex >= 0 && srcConfigIndex >= 0 {
		dstConfigs := dstPlugins.Content[dstConfigIndex+1]
		srcConfigs := srcPlugins.Content[srcConfigIndex+1]
		pruneMissingMapKeysRecursively(dstConfigs, srcConfigs)
		if dstConfigs != nil && dstConfigs.Kind == yaml.MappingNode && len(dstConfigs.Content) == 0 &&
			srcConfigs != nil && srcConfigs.Kind == yaml.MappingNode && len(srcConfigs.Content) == 0 {
			removeMapKey(dstPlugins, "configs")
		}
	}
}

func pruneMissingMapKeysRecursively(dst, src *yaml.Node) {
	if dst == nil || src == nil || dst.Kind != src.Kind {
		return
	}
	// Plugin settings recurse through mappings and sequences; other kinds stay unchanged.
	//goland:noinspection GoSwitchMissingCasesForIotaConsts
	switch src.Kind {
	case yaml.MappingNode:
		pruneMissingMapKeys(dst, src)
		for i := 0; i+1 < len(src.Content); i += 2 {
			if index := findMapKeyIndex(dst, src.Content[i].Value); index >= 0 {
				pruneMissingMapKeysRecursively(dst.Content[index+1], src.Content[i+1])
			}
		}
	case yaml.SequenceNode:
		for i := 0; i < len(dst.Content) && i < len(src.Content); i++ {
			pruneMissingMapKeysRecursively(dst.Content[i], src.Content[i])
		}
	}
}

func pruneMissingMapKeys(dstMap, srcMap *yaml.Node) {
	if dstMap == nil || srcMap == nil || dstMap.Kind != yaml.MappingNode || srcMap.Kind != yaml.MappingNode {
		return
	}
	keep := make(map[string]struct{}, len(srcMap.Content)/2)
	for i := 0; i+1 < len(srcMap.Content); i += 2 {
		keyNode := srcMap.Content[i]
		if keyNode == nil {
			continue
		}
		key := strings.TrimSpace(keyNode.Value)
		if key == "" {
			continue
		}
		keep[key] = struct{}{}
	}
	for i := 0; i+1 < len(dstMap.Content); {
		keyNode := dstMap.Content[i]
		if keyNode == nil {
			i += 2
			continue
		}
		key := strings.TrimSpace(keyNode.Value)
		if _, ok := keep[key]; !ok {
			dstMap.Content = append(dstMap.Content[:i], dstMap.Content[i+2:]...)
			continue
		}
		i += 2
	}
}

// normalizeCollectionNodeStyles forces YAML collections to use block notation, keeping
// lists and maps readable. Empty sequences retain flow style ([]) so empty list markers
// remain compact.
func normalizeCollectionNodeStyles(node *yaml.Node) {
	if node == nil {
		return
	}
	switch node.Kind {
	case yaml.MappingNode:
		node.Style = 0
		for i := range node.Content {
			normalizeCollectionNodeStyles(node.Content[i])
		}
	case yaml.SequenceNode:
		if len(node.Content) == 0 {
			node.Style = yaml.FlowStyle
		} else {
			node.Style = 0
		}
		for i := range node.Content {
			normalizeCollectionNodeStyles(node.Content[i])
		}
	default:
		// Scalars keep their existing style to preserve quoting
	}
}

// isKnownDefaultValue returns true if the given node at the specified path
// represents a known default value that should not be written to the config file.
// This prevents non-zero defaults from polluting the config.
func isKnownDefaultValue(path []string, node *yaml.Node) bool {
	if len(path) == 1 && path[0] == "plugins" && node != nil && node.Kind == yaml.MappingNode {
		if index := findMapKeyIndex(node, "configs"); index >= 0 && index+1 < len(node.Content) {
			configs := node.Content[index+1]
			if configs != nil && configs.Kind == yaml.MappingNode && len(configs.Content) > 0 {
				return false
			}
		}
	}
	if len(path) >= 2 && path[0] == "plugins" && path[1] == "configs" {
		return false
	}
	if len(path) == 2 && path[0] == "claude-api-key" && path[1] == "cloak" &&
		node != nil && node.Kind == yaml.MappingNode {
		return false
	}
	if len(path) == 3 && path[0] == "claude-api-key" && path[1] == "cloak" &&
		path[2] == "cache-user-id" && node != nil && node.Tag == "!!bool" {
		return false
	}
	// 凭证覆盖项的 false 和零带有明确语义，不能按全局默认值省略
	if len(path) == 2 && node != nil && node.Kind == yaml.ScalarNode {
		switch path[0] {
		case "claude-api-key",
			"codex-api-key",
			"xai-api-key",
			"gemini-api-key",
			"interactions-api-key",
			"vertex-api-key",
			"meta-api-key",
			"openai-compatibility":
			if (path[1] == "disable-cooling" && node.Tag == "!!bool") ||
				(path[0] == "codex-api-key" && path[1] == "disable-codex-cloaking" && node.Tag == "!!bool") ||
				(path[0] == "vertex-api-key" && path[1] == "interactions" && node.Tag == "!!bool") ||
				(path[1] == "request-retry" && node.Tag == "!!int") {
				return false
			}
		}
	}

	// OpenAI 兼容模型在源文件中显式声明请求 token 上限字段的选择
	if len(path) == 3 && path[0] == "openai-compatibility" && path[1] == "models" &&
		path[2] == "use-max-completion-tokens" && node != nil && node.Tag == "!!bool" {
		return false
	}

	// Weight is pointer-backed, so an explicit zero is meaningful and must be preserved.
	if len(path) > 0 && path[len(path)-1] == "weight" && node != nil && node.Kind == yaml.ScalarNode &&
		node.Tag == "!!int" {
		return false
	}

	// Antigravity connection-pool pointer fields preserve explicit false and zero values.
	if len(path) == 1 && path[0] == "antigravity" && node != nil && node.Kind == yaml.MappingNode {
		if poolIndex := findMapKeyIndex(node, "connection-pool"); poolIndex >= 0 &&
			poolIndex+1 < len(node.Content) {
			poolNode := node.Content[poolIndex+1]
			if poolNode != nil && poolNode.Kind == yaml.MappingNode &&
				(findMapKeyIndex(poolNode, "enabled") >= 0 || findMapKeyIndex(
					poolNode, "max-idle-conns-per-host",
				) >= 0) {
				return false
			}
		}
	}
	if len(path) == 2 && path[0] == "antigravity" && path[1] == "connection-pool" &&
		node != nil && node.Kind == yaml.MappingNode &&
		(findMapKeyIndex(node, "enabled") >= 0 || findMapKeyIndex(node, "max-idle-conns-per-host") >= 0) {
		return false
	}
	if len(path) == 3 && path[0] == "antigravity" && path[1] == "connection-pool" &&
		node != nil && node.Kind == yaml.ScalarNode {
		if (path[2] == "enabled" && node.Tag == "!!bool") ||
			(path[2] == "max-idle-conns-per-host" && node.Tag == "!!int") {
			return false
		}
	}

	// Some omitted fields reload to a non-zero default. Preserve explicit zero or false values there.
	if node == nil {
		return true
	}
	fullPath := strings.Join(path, ".")
	switch fullPath {
	case "ws-auth":
		return node.Tag == "!!bool" && node.Value == "true"
	case "remote-management", "routing", "image-artifact-cache", "claude-header-defaults":
		return node.Kind == yaml.MappingNode && len(node.Content) == 0
	case "remote-management.disable-auto-update-panel", "remote-management.auto-update-panel",
		"routing.session-affinity-subagents", "claude-header-defaults.stabilize-device-profile":
		return false // These values are emitted only when set, including false.
	case "credential-in-flight.max-details":
		return node.Tag == "!!int" && node.Value == strconv.Itoa(DefaultInFlightMaxDetails)
	case "auto-refresh-interval":
		return node.Tag == "!!int" && node.Value == strconv.Itoa(defaultAutoRefreshInterval)
	case "model-refresh-interval":
		return node.Tag == "!!int" && node.Value == strconv.Itoa(defaultModelRefreshInterval)
	case "error-logs-max-files":
		return node.Tag == "!!int" && node.Value == strconv.Itoa(defaultErrorLogsMaxFiles)
	case "image-artifact-cache.retention-days":
		return node.Tag == "!!int" && node.Value == strconv.Itoa(defaultImageArtifactRetentionDays)
	case "image-artifact-cache.max-total-size-mb":
		return node.Tag == "!!int" && node.Value == strconv.Itoa(defaultImageArtifactCacheSizeMB)
	}
	if isZeroValueNode(node) {
		return true
	}

	// Match known non-zero defaults by exact dotted path.

	// Check string defaults
	if node.Kind == yaml.ScalarNode && node.Tag == "!!str" {
		switch fullPath {
		case "pprof.addr":
			return node.Value == DefaultPprofAddr
		case "remote-management.panel-github-repository":
			return node.Value == DefaultPanelGitHubRepository
		case "remote-management.cpa-github-repository":
			return node.Value == defaultCPAGitHubRepository
		case "routing.strategy":
			return node.Value == "round-robin"
		}
	}

	return false
}

// pruneKnownDefaultsInNewNode removes default-valued descendants from a new node
// before it is appended into the destination YAML tree.
func pruneKnownDefaultsInNewNode(path []string, node *yaml.Node) {
	if node == nil || len(path) >= 2 && path[0] == "plugins" && path[1] == "configs" {
		return
	}

	switch node.Kind {
	case yaml.MappingNode:
		filtered := make([]*yaml.Node, 0, len(node.Content))
		for i := 0; i+1 < len(node.Content); i += 2 {
			keyNode := node.Content[i]
			valueNode := node.Content[i+1]
			if keyNode == nil || valueNode == nil {
				continue
			}

			childPath := appendPath(path, keyNode.Value)
			if isKnownDefaultValue(childPath, valueNode) {
				continue
			}

			pruneKnownDefaultsInNewNode(childPath, valueNode)
			if isKnownDefaultValue(childPath, valueNode) {
				continue
			}

			filtered = append(filtered, keyNode, valueNode)
		}
		node.Content = filtered
	case yaml.SequenceNode:
		for _, child := range node.Content {
			pruneKnownDefaultsInNewNode(path, child)
		}
	default:
	}
}

// appendPath appends a key to the path, returning a new slice to avoid modifying the original.
func appendPath(path []string, key string) []string {
	if len(path) == 0 {
		return []string{key}
	}
	newPath := make([]string, len(path)+1)
	copy(newPath, path)
	newPath[len(path)] = key
	return newPath
}

func mappingScalarValue(node *yaml.Node, key string) string {
	if node == nil || node.Kind != yaml.MappingNode {
		return ""
	}
	lowerKey := strings.ToLower(key)
	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode := node.Content[i]
		valNode := node.Content[i+1]
		if keyNode == nil || valNode == nil || valNode.Kind != yaml.ScalarNode {
			continue
		}
		if strings.ToLower(strings.TrimSpace(keyNode.Value)) == lowerKey {
			return strings.TrimSpace(valNode.Value)
		}
	}
	return ""
}

// Only typed fields are removed when omitted; extensions remain on their original entry.
func pruneMissingConfigMapKeys(dst, src *yaml.Node, path []string) error {
	if len(path) >= 2 && path[0] == "plugins" && path[1] == "configs" {
		pruneMissingMapKeys(dst, src)
		return nil
	}
	t := reflect.TypeFor[Config]()
	for _, key := range path {
		t = configYAMLFieldType(t, key)
	}
	defaults := yamlRecordDefaults(path)
	for i := 0; i+1 < len(dst.Content); {
		key, value := dst.Content[i].Value, dst.Content[i+1]
		emptyCollection := (value.Kind == yaml.MappingNode || value.Kind == yaml.SequenceNode) && len(value.Content) == 0
		if findMapKeyIndex(src, key) < 0 && configYAMLFieldType(t, key) != nil &&
			value.Tag != "!!null" && !emptyCollection {
			if index := slices.IndexFunc(
				defaults, func(field yamlDefault) bool { return field.key == key },
			); index >= 0 {
				field := defaults[index]
				if err := mergeNodePreserve(
					value, &yaml.Node{Kind: yaml.ScalarNode, Tag: field.tag, Value: field.value}, false,
				); err != nil {
					return err
				}
			} else {
				dst.Content = append(dst.Content[:i], dst.Content[i+2:]...)
				continue
			}
		}
		i += 2
	}
	return nil
}

func configYAMLFieldType(t reflect.Type, key string) reflect.Type {
	for t != nil && (t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array) {
		t = t.Elem()
	}
	if t == nil {
		return nil
	}
	if t.Kind() == reflect.Map {
		return t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	for field := range t.Fields() {
		tag := field.Tag.Get("yaml")
		name, _, _ := strings.Cut(tag, ",")
		if name == key {
			return field.Type
		}
		if strings.Contains(tag, ",inline") {
			if found := configYAMLFieldType(field.Type, key); found != nil {
				return found
			}
		}
	}
	return nil
}
