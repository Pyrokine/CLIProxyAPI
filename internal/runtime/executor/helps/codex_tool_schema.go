package helps

import (
	"bytes"
	"encoding/json"
	"io"
	"math/big"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const codexComplexUnionBranchThreshold = 8

// NormalizeCodexToolSchemas simplifies pure constant unions in Codex tool schemas
// and removes pattern escapes unsupported by upstream schema validators.
func NormalizeCodexToolSchemas(body []byte) []byte {
	updatedTools, changed := normalizeCodexToolList(gjson.GetBytes(body, "tools"))
	if !changed {
		return body
	}
	updatedBody, errSet := sjson.SetRawBytes(body, "tools", updatedTools)
	if errSet != nil {
		return body
	}
	log.Debug("codex: normalized tool schemas to prevent upstream failure")
	return updatedBody
}

func normalizeCodexToolList(tools gjson.Result) ([]byte, bool) {
	if !tools.IsArray() {
		return nil, false
	}
	var updated []byte
	offset := 0
	tools.ForEach(func(_, tool gjson.Result) bool {
		updatedTool, changed := normalizeCodexTool(tool)
		if !changed {
			return true
		}
		if updated == nil {
			updated = make([]byte, 0, len(tools.Raw))
		}
		start := tool.Index - tools.Index
		updated = append(updated, tools.Raw[offset:start]...)
		updated = append(updated, updatedTool...)
		offset = start + len(tool.Raw)
		return true
	})
	if updated == nil {
		return nil, false
	}
	return append(updated, tools.Raw[offset:]...), true
}

func normalizeCodexTool(tool gjson.Result) ([]byte, bool) {
	switch tool.Get("type").String() {
	case "namespace":
		updatedTools, changed := normalizeCodexToolList(tool.Get("tools"))
		if !changed {
			return nil, false
		}
		updated, errSet := sjson.SetRawBytes([]byte(tool.Raw), "tools", updatedTools)
		return updated, errSet == nil
	case "function", "custom":
	default:
		return nil, false
	}

	params := tool.Get("parameters")
	if !params.Exists() || !params.IsObject() {
		return nil, false
	}
	updatedParams, paramsChanged := normalizeCodexParameters(params)
	if !paramsChanged {
		return nil, false
	}
	updatedTool, errSet := sjson.SetRawBytes([]byte(tool.Raw), "parameters", updatedParams)
	if errSet != nil {
		return nil, false
	}
	return updatedTool, true
}

func normalizeCodexParameters(params gjson.Result) ([]byte, bool) {
	rawParams := []byte(params.Raw)
	changed := false
	if sanitizedParams, patternChanged := stripCodexIncompatiblePatterns(rawParams); patternChanged {
		rawParams = sanitizedParams
		params = gjson.ParseBytes(rawParams)
		changed = true
	}

	properties := params.Get("properties")
	if !properties.Exists() || !properties.IsObject() {
		return rawParams, changed
	}
	for propertyName, property := range properties.Map() {
		updatedProperty, propertyChanged := normalizeCodexPropertySchema(property)
		if !propertyChanged {
			continue
		}
		var errSet error
		rawParams, errSet = sjson.SetRawBytes(
			rawParams,
			"properties."+escapeCodexSJSONKey(propertyName),
			updatedProperty,
		)
		if errSet == nil {
			changed = true
		}
	}
	return rawParams, changed
}

func stripCodexIncompatiblePatterns(raw []byte) ([]byte, bool) {
	rawText := string(raw)
	if !strings.Contains(rawText, `\p{`) &&
		!strings.Contains(rawText, `\P{`) &&
		!strings.Contains(rawText, `\u`) &&
		!strings.Contains(rawText, `\\0`) {
		return raw, false
	}

	var root any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if errDecode := decoder.Decode(&root); errDecode != nil || root == nil {
		return raw, false
	}
	var trailing any
	if errTrailing := decoder.Decode(&trailing); errTrailing != io.EOF {
		return raw, false
	}
	if !stripCodexIncompatiblePatternsFromSchema(root) {
		return raw, false
	}

	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if errEncode := encoder.Encode(root); errEncode != nil {
		return raw, false
	}
	return bytes.TrimSpace(buffer.Bytes()), true
}

func stripCodexIncompatiblePatternsFromSchema(value any) bool {
	changed := false
	switch schema := value.(type) {
	case map[string]any:
		if pattern, ok := schema["pattern"].(string); ok && util.HasUnsupportedUnicodePropertyEscape(pattern) {
			delete(schema, "pattern")
			changed = true
		}

		if patternProperties, ok := schema["patternProperties"].(map[string]any); ok {
			for pattern, child := range patternProperties {
				if util.HasUnsupportedUnicodePropertyEscape(pattern) {
					delete(patternProperties, pattern)
					changed = true
					continue
				}
				if stripCodexIncompatiblePatternsFromSchema(child) {
					changed = true
				}
			}
		}

		for _, keyword := range util.SchemaMapKeywords {
			if keyword == "patternProperties" {
				continue
			}
			children, ok := schema[keyword].(map[string]any)
			if !ok {
				continue
			}
			for _, child := range children {
				if stripCodexIncompatiblePatternsFromSchema(child) {
					changed = true
				}
			}
		}
		for _, keyword := range util.SchemaValueKeywords {
			child, exists := schema[keyword]
			if exists && stripCodexIncompatiblePatternsFromSchema(child) {
				changed = true
			}
		}
	case []any:
		for _, child := range schema {
			if stripCodexIncompatiblePatternsFromSchema(child) {
				changed = true
			}
		}
	}
	return changed
}

func normalizeCodexPropertySchema(property gjson.Result) ([]byte, bool) {
	if !property.IsObject() {
		return nil, false
	}

	hasOneOf := property.Get("oneOf").Exists()
	hasAnyOf := property.Get("anyOf").Exists()
	if hasOneOf == hasAnyOf {
		return nil, false
	}
	unionName := "oneOf"
	if hasAnyOf {
		unionName = "anyOf"
	}
	union := property.Get(unionName)
	if !union.IsArray() || len(union.Array()) < codexComplexUnionBranchThreshold {
		return nil, false
	}

	rawValues := make([]string, 0, len(union.Array()))
	semanticKeys := make([]string, 0, len(union.Array()))
	seen := make(map[string]struct{}, len(union.Array()))
	for _, branch := range union.Array() {
		semanticKey, rawValue, ok := codexPureConstBranch(branch)
		if !ok {
			return nil, false
		}
		if _, duplicate := seen[semanticKey]; duplicate {
			return nil, false
		}
		seen[semanticKey] = struct{}{}
		semanticKeys = append(semanticKeys, semanticKey)
		rawValues = append(rawValues, rawValue)
	}

	rawProperty := []byte(property.Raw)
	if existingEnum := property.Get("enum"); existingEnum.Exists() && existingEnum.IsArray() {
		existingKeys := make([]string, 0, len(existingEnum.Array()))
		for _, value := range existingEnum.Array() {
			key, ok := canonicalCodexJSONValueKey(value)
			if !ok {
				return nil, false
			}
			existingKeys = append(existingKeys, key)
		}
		if !equalCodexCanonicalSets(existingKeys, semanticKeys) {
			return nil, false
		}
		updated, errDelete := sjson.DeleteBytes(rawProperty, unionName)
		return updated, errDelete == nil
	}

	updated, errEnum := sjson.SetRawBytes(rawProperty, "enum", []byte("["+strings.Join(rawValues, ",")+"]"))
	if errEnum != nil {
		return nil, false
	}
	updated, errDelete := sjson.DeleteBytes(updated, unionName)
	return updated, errDelete == nil
}

func codexPureConstBranch(branch gjson.Result) (string, string, bool) {
	if !branch.IsObject() {
		return "", "", false
	}
	constValue := branch.Get("const")
	if !constValue.Exists() {
		return "", "", false
	}
	for key := range branch.Map() {
		if key != "const" && key != "description" && key != "title" {
			return "", "", false
		}
	}
	semanticKey, ok := canonicalCodexJSONValueKey(constValue)
	if !ok {
		return "", "", false
	}
	return semanticKey, constValue.Raw, true
}

func canonicalCodexJSONValueKey(value gjson.Result) (string, bool) {
	switch value.Type {
	case gjson.String:
		return "s:" + value.String(), true
	case gjson.Number:
		raw := strings.TrimSpace(value.Raw)
		var rational big.Rat
		if _, ok := rational.SetString(raw); ok {
			return "n:" + rational.RatString(), true
		}
		return "n:" + raw, true
	case gjson.True:
		return "b:true", true
	case gjson.False:
		return "b:false", true
	case gjson.Null:
		return "null", true
	default:
		return "", false
	}
}

func equalCodexCanonicalSets(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	leftSet := make(map[string]struct{}, len(left))
	for _, value := range left {
		leftSet[value] = struct{}{}
	}
	for _, value := range right {
		if _, ok := leftSet[value]; !ok {
			return false
		}
	}
	return len(leftSet) == len(left)
}

func escapeCodexSJSONKey(key string) string {
	key = strings.ReplaceAll(key, `\`, `\\`)
	key = strings.ReplaceAll(key, `.`, `\.`)
	return strings.ReplaceAll(key, `:`, `\:`)
}
