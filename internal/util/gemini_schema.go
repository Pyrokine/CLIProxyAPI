// Package util provides utility functions for the CLI Proxy API server.
package util

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var gjsonPathKeyReplacer = strings.NewReplacer(".", "\\.", "*", "\\*", "?", "\\?")

const placeholderReasonDescription = "Brief explanation of why you are calling this tool"

// Pass a single JSON schema to the functions below, never a whole request document.
//
// Cleaning walks every node and rewrites keys by name. Schema keywords such as "title",
// "format", "default", and "const" are also ordinary data keys. Passing a request silently
// rewrites tool-call arguments in conversation history, so every call site must select the schema.

type jsonSchemaCleanOptions struct {
	addPlaceholder                    bool
	addMissingArrayItems              bool
	removeGeminiMetadata              bool
	flattenUnions                     bool
	inferEnumStringType               bool
	preserveAdditionalPropertiesFalse bool
	preserveStandardConstraints       bool
}

// CleanJSONSchemaForAntigravity transforms a tool schema to be compatible with Antigravity API.
// It handles unsupported keywords, type flattening, and schema simplification while preserving
// semantic information as description hints and adding placeholders required by VALIDATED mode.
func CleanJSONSchemaForAntigravity(jsonStr string) string {
	return cleanJSONSchema(
		jsonStr, jsonSchemaCleanOptions{
			addPlaceholder:       true,
			addMissingArrayItems: true,
			flattenUnions:        true,
			inferEnumStringType:  true,
		},
	)
}

// CleanJSONSchemaForAntigravityJSONSchema preserves constraints in the parametersJsonSchema carrier.
func CleanJSONSchemaForAntigravityJSONSchema(jsonStr string) string {
	return cleanJSONSchema(
		jsonStr, jsonSchemaCleanOptions{
			addPlaceholder:              true,
			addMissingArrayItems:        true,
			preserveStandardConstraints: true,
		},
	)
}

// CleanJSONSchemaForAntigravityResponse transforms a response schema without applying tool-only
// compatibility rewrites that would alter the client's structured output contract.
func CleanJSONSchemaForAntigravityResponse(jsonStr string) string {
	return cleanJSONSchema(
		jsonStr,
		jsonSchemaCleanOptions{preserveAdditionalPropertiesFalse: true},
	)
}

// CleanJSONSchemaForGemini transforms a JSON schema to be compatible with Gemini tool calling.
// It removes unsupported keywords and simplifies schemas, without adding empty-schema placeholders.
func CleanJSONSchemaForGemini(jsonStr string) string {
	return cleanJSONSchema(
		jsonStr, jsonSchemaCleanOptions{
			addMissingArrayItems: true,
			removeGeminiMetadata: true,
			flattenUnions:        true,
			inferEnumStringType:  true,
		},
	)
}

// CleanJSONSchemaForGeminiJSONSchema preserves standard constraints in the parametersJsonSchema carrier.
func CleanJSONSchemaForGeminiJSONSchema(jsonStr string) string {
	return cleanJSONSchema(
		jsonStr, jsonSchemaCleanOptions{
			addMissingArrayItems:        true,
			removeGeminiMetadata:        true,
			preserveStandardConstraints: true,
		},
	)
}

// cleanJSONSchema performs the core cleaning operations on the JSON schema.
func cleanJSONSchema(jsonStr string, options jsonSchemaCleanOptions) string {
	// Phase 0: Repair malformed schema nodes before keyword-based transforms.
	jsonStr = normalizeMalformedSchemaObjects(jsonStr, options)
	if options.preserveStandardConstraints {
		return jsonStr
	}

	// Phase 1: Convert and add hints
	jsonStr = convertRefsToHints(jsonStr)
	jsonStr = convertConstToEnum(jsonStr)
	jsonStr = convertEnumValuesToStrings(jsonStr, options.inferEnumStringType)
	jsonStr = addEnumHints(jsonStr)
	if !options.preserveAdditionalPropertiesFalse {
		jsonStr = addAdditionalPropertiesHints(jsonStr)
	}
	jsonStr = moveConstraintsToDescription(jsonStr)

	// Phase 2: Flatten complex structures
	jsonStr = mergeConditionals(jsonStr)
	jsonStr = mergeAllOf(jsonStr)
	jsonStr = mergeObjectUnions(jsonStr)
	jsonStr = flattenAnyOfOneOf(jsonStr, options.flattenUnions)
	jsonStr = flattenTypeArrays(jsonStr)

	// Phase 3: Cleanup
	jsonStr = removeUnsupportedKeywords(jsonStr, options)
	if options.removeGeminiMetadata {
		// nullable 保留必填字段允许 null 的语义
		jsonStr = removeKeywords(jsonStr, []string{"title"})
		jsonStr = removePlaceholderFields(jsonStr)
	}
	jsonStr = cleanupRequiredFields(jsonStr)
	jsonStr = sanitizeObjectProperties(jsonStr)
	jsonStr = sanitizeArrayItems(jsonStr)
	// Phase 4: Add placeholder for empty object schemas (Claude VALIDATED mode requirement)
	if options.addPlaceholder {
		jsonStr = addEmptySchemaPlaceholder(jsonStr)
	}

	return jsonStr
}

// sanitizeArrayItems keeps items only on array nodes accepted by Gemini's schema validator.
func sanitizeArrayItems(jsonStr string) string {
	paths := findPaths(jsonStr, "items")
	sortByDepth(paths)
	for _, path := range paths {
		parent := trimSuffix(path, ".items")
		if isPropertyDefinition(parent) {
			continue
		}
		typePath := joinPath(parent, "type")
		declaredType := gjson.Get(jsonStr, typePath).String()
		if declaredType == "" {
			updated, _ := sjson.SetBytes([]byte(jsonStr), typePath, "array")
			jsonStr = string(updated)
		} else if !strings.EqualFold(declaredType, "array") {
			jsonStr, _ = sjson.Delete(jsonStr, path)
		}
	}
	return jsonStr
}

// sanitizeObjectProperties keeps properties on object nodes accepted by Gemini's schema validator.
func sanitizeObjectProperties(jsonStr string) string {
	paths := findPaths(jsonStr, "properties")
	sortByDepth(paths)
	for _, path := range paths {
		parent := trimSuffix(path, ".properties")
		if isPropertyDefinition(parent) || !gjson.Get(jsonStr, path).IsObject() {
			continue
		}
		typePath := joinPath(parent, "type")
		if !strings.EqualFold(gjson.Get(jsonStr, typePath).String(), "object") {
			updated, _ := sjson.SetBytes([]byte(jsonStr), typePath, "object")
			jsonStr = string(updated)
		}
	}
	return jsonStr
}

// removeKeywords removes all occurrences of specified keywords from the JSON schema.
func removeKeywords(jsonStr string, keywords []string) string {
	deletePaths := make([]string, 0)
	pathsByField := findPathsByFields(jsonStr, keywords)
	for _, key := range keywords {
		for _, p := range pathsByField[key] {
			if isPropertyDefinition(trimSuffix(p, "."+key)) {
				continue
			}
			deletePaths = append(deletePaths, p)
		}
	}
	sortByDepth(deletePaths)
	for _, p := range deletePaths {
		jsonStr, _ = sjson.Delete(jsonStr, p)
	}
	return jsonStr
}

// removePlaceholderFields removes placeholder-only properties ("_" and "reason") and their required entries.
func removePlaceholderFields(jsonStr string) string {
	// Remove "_" placeholder properties.
	paths := findPaths(jsonStr, "_")
	sortByDepth(paths)
	for _, p := range paths {
		if !strings.HasSuffix(p, ".properties._") {
			continue
		}
		jsonStr, _ = sjson.Delete(jsonStr, p)
		parentPath := trimSuffix(p, ".properties._")
		reqPath := joinPath(parentPath, "required")
		req := gjson.Get(jsonStr, reqPath)
		if req.IsArray() {
			var filtered []string
			for _, r := range req.Array() {
				if r.String() != "_" {
					filtered = append(filtered, r.String())
				}
			}
			if len(filtered) == 0 {
				jsonStr, _ = sjson.Delete(jsonStr, reqPath)
			} else {
				updated, _ := sjson.SetBytes([]byte(jsonStr), reqPath, filtered)
				jsonStr = string(updated)
			}
		}
	}

	// Remove placeholder-only "reason" objects.
	reasonPaths := findPaths(jsonStr, "reason")
	sortByDepth(reasonPaths)
	for _, p := range reasonPaths {
		if !strings.HasSuffix(p, ".properties.reason") {
			continue
		}
		parentPath := trimSuffix(p, ".properties.reason")
		props := gjson.Get(jsonStr, joinPath(parentPath, "properties"))
		if !props.IsObject() || len(props.Map()) != 1 {
			continue
		}
		desc := gjson.Get(jsonStr, p+".description").String()
		if desc != placeholderReasonDescription {
			continue
		}
		jsonStr, _ = sjson.Delete(jsonStr, p)
		reqPath := joinPath(parentPath, "required")
		req := gjson.Get(jsonStr, reqPath)
		if req.IsArray() {
			var filtered []string
			for _, r := range req.Array() {
				if r.String() != "reason" {
					filtered = append(filtered, r.String())
				}
			}
			if len(filtered) == 0 {
				jsonStr, _ = sjson.Delete(jsonStr, reqPath)
			} else {
				updated, _ := sjson.SetBytes([]byte(jsonStr), reqPath, filtered)
				jsonStr = string(updated)
			}
		}
	}

	return jsonStr
}

// normalizeMalformedSchemaObjects repairs malformed schema shapes emitted by
// tool providers while leaving complete API request documents untouched.
func normalizeMalformedSchemaObjects(jsonStr string, options jsonSchemaCleanOptions) string {
	trimmed := strings.TrimSpace(jsonStr)
	if trimmed == "" || trimmed == "null" {
		return emptyClaudeToolInputSchema
	}

	root, ok := decodeSchemaJSON(trimmed)
	if !ok {
		return jsonStr
	}
	if rootBool, ok := root.(bool); ok && rootBool {
		return "{}"
	}
	rootMap, rootWasStringified := schemaMapFromValue(root)
	if rootMap == nil || isAPIRequestDocument(rootMap) {
		return jsonStr
	}

	if len(rootMap) == 1 {
		if inner, innerWasStringified := schemaMapFromValue(rootMap["schema"]); inner != nil {
			repairedInner, modified := repairSchemaNode(inner, options, true)
			if !modified && !innerWasStringified && !rootWasStringified {
				return jsonStr
			}
			out, err := marshalJSONNoHTMLEscape(map[string]any{"schema": repairedInner})
			if err != nil {
				return jsonStr
			}
			return string(out)
		} else if inner, ok := rootMap["schema"].(bool); ok && inner {
			out, err := marshalJSONNoHTMLEscape(map[string]any{"schema": map[string]any{}})
			if err != nil {
				return jsonStr
			}
			return string(out)
		}
	}

	repaired, modified := repairSchemaNode(rootMap, options, false)
	if !modified && !rootWasStringified {
		return jsonStr
	}
	out, err := marshalJSONNoHTMLEscape(repaired)
	if err != nil {
		return jsonStr
	}
	return string(out)
}

func decodeSchemaJSON(raw string) (any, bool) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, false
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, false
	}
	return value, true
}

func schemaMapFromValue(value any) (map[string]any, bool) {
	switch typed := value.(type) {
	case map[string]any:
		return typed, false
	case string:
		decoded, ok := decodeSchemaJSON(strings.TrimSpace(typed))
		if !ok {
			return nil, false
		}
		if schema, ok := decoded.(map[string]any); ok {
			return schema, true
		}
	}
	return nil, false
}

func marshalJSONNoHTMLEscape(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte{'\n'}), nil
}

func isKnownSchemaKeywordOrExtension(key string) bool {
	if strings.HasPrefix(key, "x-") {
		return true
	}
	switch key {
	case "properties", "patternProperties", "additionalProperties", "items", "prefixItems",
		"$defs", "definitions", "dependentSchemas", "dependentRequired", "dependencies",
		"if", "then", "else", "not", "contains", "propertyNames",
		"unevaluatedProperties", "unevaluatedItems", "contentSchema", "additionalItems",
		"default", "const", "example", "examples", "discriminator", "xml", "externalDocs",
		"enumDescriptions", "enumTitles":
		return true
	default:
		return false
	}
}

func isNonObjectDeclaredType(value any) bool {
	switch typed := value.(type) {
	case string:
		return typed != "" && !strings.EqualFold(typed, "object")
	case []any:
		for _, item := range typed {
			if itemType, ok := item.(string); ok && strings.EqualFold(itemType, "object") {
				return false
			}
		}
		return len(typed) > 0
	default:
		return false
	}
}

func isArrayDeclaredType(value any) bool {
	switch typed := value.(type) {
	case string:
		return strings.EqualFold(typed, "array")
	case []any:
		for _, item := range typed {
			if itemType, ok := item.(string); ok && strings.EqualFold(itemType, "array") {
				return true
			}
		}
	}
	return false
}

func isAPIRequestDocument(value map[string]any) bool {
	for _, key := range []string{"tools", "contents", "messages", "functionDeclarations", "function_declarations"} {
		if _, ok := value[key].([]any); ok {
			return true
		}
	}
	if request, ok := value["request"].(map[string]any); ok {
		return isAPIRequestDocument(request)
	}
	return false
}

func repairSchemaNode(node map[string]any, options jsonSchemaCleanOptions, nested bool) (map[string]any, bool) {
	if node == nil {
		return nil, false
	}

	clone := make(map[string]any, len(node))
	maps.Copy(clone, node)
	modified := false
	if options.preserveStandardConstraints {
		for key := range clone {
			remove := strings.HasPrefix(key, "x-")
			switch key {
			case "$schema", "$id", "id", "$comment", "encrypted", "prefill", "deprecated",
				"enumDescriptions", "enumTitles", "propertyNames", "patternProperties":
				remove = true
			case "title":
				remove = options.removeGeminiMetadata
			}
			if remove {
				delete(clone, key)
				modified = true
			}
		}
		if nullable, ok := clone["nullable"].(bool); ok {
			delete(clone, "nullable")
			modified = true
			if nullable {
				switch declared := clone["type"].(type) {
				case string:
					if declared != "null" {
						clone["type"] = []any{declared, "null"}
					}
				case []any:
					if !slices.Contains(declared, "null") {
						clone["type"] = append(slices.Clone(declared), "null")
					}
				}
			}
		}
	}

	if !isNonObjectDeclaredType(clone["type"]) {
		bareProperties := make(map[string]any)
		for key, value := range clone {
			if isKnownSchemaKeywordOrExtension(key) {
				continue
			}
			if child, ok := value.(map[string]any); ok {
				bareProperties[key] = child
			}
		}
		if len(bareProperties) > 0 {
			repairedProperties, promotedRequired, _ := repairPropertyMap(bareProperties, options)
			for key := range bareProperties {
				delete(clone, key)
			}
			if existing, ok := clone["properties"].(map[string]any); ok {
				merged := make(map[string]any, len(existing)+len(repairedProperties))
				maps.Copy(merged, existing)
				maps.Copy(merged, repairedProperties)
				clone["properties"] = merged
			} else {
				clone["properties"] = repairedProperties
				if _, hasType := clone["type"]; !hasType {
					clone["type"] = "object"
				}
			}
			if len(promotedRequired) > 0 {
				clone["required"] = mergeStringSlices(extractStringArray(clone["required"]), promotedRequired)
			}
			modified = true
		}
	}

	if properties, ok := clone["properties"].(map[string]any); ok {
		repairedProperties, promotedRequired, propertiesModified := repairPropertyMap(
			properties,
			options,
		)
		if propertiesModified {
			clone["properties"] = repairedProperties
			modified = true
		}
		if len(promotedRequired) > 0 {
			clone["required"] = mergeStringSlices(extractStringArray(clone["required"]), promotedRequired)
			modified = true
		}
	}

	if options.addMissingArrayItems {
		if isArrayDeclaredType(clone["type"]) {
			if _, hasItems := clone["items"]; !hasItems && !options.preserveStandardConstraints {
				clone["items"] = map[string]any{"type": "string"}
				modified = true
			}
		} else if _, hasItems := clone["items"]; hasItems {
			if clone["type"] == nil || clone["type"] == "" {
				clone["type"] = "array"
				modified = true
			} else if options.preserveStandardConstraints {
				delete(clone, "items")
				modified = true
			}
		}
	}

	if items, stringified := schemaMapFromValue(clone["items"]); items != nil {
		repairedItems, itemsModified := repairSchemaNode(items, options, true)
		if itemsModified || stringified {
			clone["items"] = repairedItems
			modified = true
		}
	} else if itemsList, ok := clone["items"].([]any); ok {
		repairedList, listModified := repairSchemaList(itemsList, options)
		if listModified {
			clone["items"] = repairedList
			modified = true
		}
	} else if itemsBool, ok := clone["items"].(bool); ok && itemsBool {
		clone["items"] = map[string]any{}
		modified = true
	}

	if additionalProperties, stringified := schemaMapFromValue(clone["additionalProperties"]); additionalProperties !=
		nil {
		repaired, childModified := repairSchemaNode(additionalProperties, options, true)
		if childModified || stringified {
			clone["additionalProperties"] = repaired
			modified = true
		}
	}
	if patternProperties, ok := clone["patternProperties"].(map[string]any); ok {
		repaired, _, childModified := repairPropertyMap(patternProperties, options)
		if childModified {
			clone["patternProperties"] = repaired
			modified = true
		}
	}

	for _, key := range []string{
		"if", "then", "else", "not", "contains", "propertyNames", "unevaluatedProperties",
		"unevaluatedItems", "contentSchema", "additionalItems",
	} {
		if child, stringified := schemaMapFromValue(clone[key]); child != nil {
			repaired, childModified := repairSchemaNode(child, options, true)
			if childModified || stringified {
				clone[key] = repaired
				modified = true
			}
		} else if childBool, ok := clone[key].(bool); ok && childBool {
			clone[key] = map[string]any{}
			modified = true
		}
	}
	for _, key := range []string{"anyOf", "oneOf", "allOf", "prefixItems"} {
		if list, ok := clone[key].([]any); ok {
			repaired, listModified := repairSchemaList(list, options)
			if listModified {
				clone[key] = repaired
				modified = true
			}
		}
	}
	for _, key := range []string{"$defs", "definitions", "dependentSchemas", "dependencies"} {
		definitions, ok := clone[key].(map[string]any)
		if !ok {
			continue
		}
		repairedDefinitions := make(map[string]any, len(definitions))
		definitionsModified := false
		for name, value := range definitions {
			if child, stringified := schemaMapFromValue(value); child != nil {
				repaired, childModified := repairSchemaNode(child, options, true)
				repairedDefinitions[name] = repaired
				definitionsModified = definitionsModified || childModified || stringified
				continue
			}
			if enabled, ok := value.(bool); ok && enabled {
				repairedDefinitions[name] = map[string]any{}
				definitionsModified = true
				modified = true
			} else {
				repairedDefinitions[name] = value
			}
		}
		if definitionsModified {
			clone[key] = repairedDefinitions
			modified = true
		}
	}
	// const 和 enum 中的对象是数据，占位字段只添加到 schema 节点
	if options.preserveStandardConstraints && options.addPlaceholder && clone["type"] == "object" {
		properties, _ := clone["properties"].(map[string]any)
		if len(properties) == 0 {
			clone["properties"] = map[string]any{
				"reason": map[string]any{"type": "string", "description": placeholderReasonDescription},
			}
			clone["required"] = []string{"reason"}
			modified = true
		} else if nested && len(extractStringArray(clone["required"])) == 0 {
			properties = maps.Clone(properties)
			if _, exists := properties["_"]; !exists {
				properties["_"] = map[string]any{"type": "boolean"}
			}
			clone["properties"] = properties
			clone["required"] = []string{"_"}
			modified = true
		}
	}
	return clone, modified
}

func repairSchemaList(list []any, options jsonSchemaCleanOptions) ([]any, bool) {
	repaired := make([]any, 0, len(list))
	modified := false
	for _, value := range list {
		if child, stringified := schemaMapFromValue(value); child != nil {
			repairedChild, childModified := repairSchemaNode(child, options, true)
			repaired = append(repaired, repairedChild)
			modified = modified || childModified || stringified
			continue
		}
		if enabled, ok := value.(bool); ok && enabled {
			repaired = append(repaired, map[string]any{})
			modified = true
		} else {
			repaired = append(repaired, value)
		}
	}
	return repaired, modified
}

func repairPropertyMap(
	properties map[string]any,
	options jsonSchemaCleanOptions,
) (map[string]any, []string, bool) {
	repaired := make(map[string]any, len(properties))
	promotedRequired := make([]string, 0)
	modified := false
	for name, value := range properties {
		if enabled, ok := value.(bool); ok && enabled {
			repaired[name] = map[string]any{}
			modified = true
			continue
		}
		child, stringified := schemaMapFromValue(value)
		if child == nil {
			repaired[name] = value
			continue
		}
		childClone := make(map[string]any, len(child))
		maps.Copy(childClone, child)
		if required, ok := childClone["required"].(bool); ok {
			delete(childClone, "required")
			modified = true
			if required {
				promotedRequired = append(promotedRequired, name)
			}
		}
		repairedChild, childModified := repairSchemaNode(childClone, options, true)
		repaired[name] = repairedChild
		modified = modified || childModified || stringified
	}
	sort.Strings(promotedRequired)
	return repaired, promotedRequired, modified
}

func extractStringArray(value any) []string {
	items, ok := value.([]any)
	if !ok {
		if stringsValue, ok := value.([]string); ok {
			return stringsValue
		}
		return nil
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		if stringValue, ok := item.(string); ok {
			result = append(result, stringValue)
		}
	}
	return result
}

func mergeStringSlices(existing, additional []string) []string {
	seen := make(map[string]struct{}, len(existing)+len(additional))
	merged := make([]string, 0, len(existing)+len(additional))
	for _, values := range [][]string{existing, additional} {
		for _, value := range values {
			if value == "" {
				continue
			}
			if _, exists := seen[value]; exists {
				continue
			}
			seen[value] = struct{}{}
			merged = append(merged, value)
		}
	}
	return merged
}

// convertRefsToHints inlines local definitions and describes unresolved or recursive references.
func convertRefsToHints(jsonStr string) string {
	root := gjson.Parse(jsonStr)
	var visit func(gjson.Result, map[string]bool) string
	visit = func(node gjson.Result, active map[string]bool) string {
		if node.IsArray() {
			items := make([]string, 0, len(node.Array()))
			for _, item := range node.Array() {
				items = append(items, visit(item, active))
			}
			return "[" + strings.Join(items, ",") + "]"
		}
		if !node.IsObject() {
			return node.Raw
		}
		out := node.Raw
		if ref := node.Get("$ref"); ref.Type == gjson.String {
			refValue := ref.String()
			var target gjson.Result
			if refValue == "#" {
				target = root
			} else if after, ok := strings.CutPrefix(refValue, "#/"); ok {
				segments := strings.Split(after, "/")
				for i, segment := range segments {
					segments[i] = escapeGJSONPathKey(
						strings.ReplaceAll(
							strings.ReplaceAll(segment, "~1", "/"), "~0", "~",
						),
					)
				}
				target = root.Get(strings.Join(segments, "."))
			}
			if target.IsObject() && !active[refValue] {
				active[refValue] = true
				out = visit(target, active)
				delete(active, refValue)
				node.ForEach(
					func(key, value gjson.Result) bool {
						if key.String() != "$ref" {
							out, _ = sjson.SetRaw(out, escapeGJSONPathKey(key.String()), value.Raw)
						}
						return true
					},
				)
			} else {
				out, _ = sjson.Delete(out, "$ref")
				if !gjson.Get(out, "type").Exists() {
					out, _ = sjson.Set(out, "type", "object")
				}
			}
			out = appendHintRaw(out, "See: "+refValue[strings.LastIndex(refValue, "/")+1:])
		}
		gjson.Parse(out).ForEach(
			func(key, value gjson.Result) bool {
				if value.IsObject() || value.IsArray() {
					out, _ = sjson.SetRaw(out, escapeGJSONPathKey(key.String()), visit(value, active))
				}
				return true
			},
		)
		return out
	}
	return visit(root, make(map[string]bool))
}

func convertConstToEnum(jsonStr string) string {
	for _, p := range findPaths(jsonStr, "const") {
		val := gjson.Get(jsonStr, p)
		if !val.Exists() {
			continue
		}
		enumPath := trimSuffix(p, ".const") + ".enum"
		if !gjson.Get(jsonStr, enumPath).Exists() {
			updated, _ := sjson.SetBytes([]byte(jsonStr), enumPath, []any{val.Value()})
			jsonStr = string(updated)
		}
	}
	return jsonStr
}

// convertEnumValuesToStrings 按 Proto Schema 格式编码枚举，保留显式类型
// 未声明类型的工具枚举使用 string
func convertEnumValuesToStrings(jsonStr string, inferStringType bool) string {
	for _, p := range findPaths(jsonStr, "enum") {
		arr := gjson.Get(jsonStr, p)
		if !arr.IsArray() {
			continue
		}

		var stringVals []string
		for _, item := range arr.Array() {
			stringVals = append(stringVals, item.String())
		}

		updated, _ := sjson.SetBytes([]byte(jsonStr), p, stringVals)
		jsonStr = string(updated)
		parentPath := trimSuffix(p, ".enum")
		if typePath := joinPath(parentPath, "type"); inferStringType && !gjson.Get(jsonStr, typePath).Exists() {
			updated, _ = sjson.SetBytes([]byte(jsonStr), typePath, "string")
			jsonStr = string(updated)
		}
	}
	return jsonStr
}

func addEnumHints(jsonStr string) string {
	for _, p := range findPaths(jsonStr, "enum") {
		arr := gjson.Get(jsonStr, p)
		if !arr.IsArray() {
			continue
		}
		items := arr.Array()
		if len(items) <= 1 || len(items) > 10 {
			continue
		}

		var vals []string
		for _, item := range items {
			vals = append(vals, item.String())
		}
		jsonStr = appendHint(jsonStr, trimSuffix(p, ".enum"), "Allowed: "+strings.Join(vals, ", "))
	}
	return jsonStr
}

func addAdditionalPropertiesHints(jsonStr string) string {
	for _, p := range findPaths(jsonStr, "additionalProperties") {
		if gjson.Get(jsonStr, p).Type == gjson.False {
			jsonStr = appendHint(jsonStr, trimSuffix(p, ".additionalProperties"), "No extra properties allowed")
		}
	}
	return jsonStr
}

var unsupportedConstraints = []string{
	"minLength", "maxLength", "exclusiveMinimum", "exclusiveMaximum",
	"pattern", "minItems", "maxItems", "uniqueItems", "contains", "format",
	"default", "examples", // Claude rejects these in VALIDATED mode
}

func moveConstraintsToDescription(jsonStr string) string {
	pathsByField := findPathsByFields(jsonStr, unsupportedConstraints)
	for _, key := range unsupportedConstraints {
		for _, p := range pathsByField[key] {
			val := gjson.Get(jsonStr, p)
			if !val.Exists() {
				continue
			}
			parentPath := trimSuffix(p, "."+key)
			if isPropertyDefinition(parentPath) {
				continue
			}
			value := val.String()
			if val.IsObject() || val.IsArray() {
				value = val.Raw
			}
			jsonStr = appendHint(jsonStr, parentPath, fmt.Sprintf("%s: %s", key, value))
		}
	}
	return jsonStr
}

func mergeConditionals(jsonStr string) string {
	pathsByField := findPathsByFields(jsonStr, []string{"then", "else"})
	paths := make([]string, 0, len(pathsByField["then"])+len(pathsByField["else"]))
	for _, key := range []string{"then", "else"} {
		for _, path := range pathsByField[key] {
			if !isPropertyDefinition(trimSuffix(path, "."+key)) {
				paths = append(paths, path)
			}
		}
	}
	sortByDepth(paths)

	for _, path := range paths {
		properties := gjson.Get(jsonStr, joinPath(path, "properties"))
		if !properties.IsObject() {
			continue
		}
		var parentPath string
		switch {
		case path == "then" || path == "else":
			parentPath = ""
		case strings.HasSuffix(path, ".then"):
			parentPath = trimSuffix(path, ".then")
		case strings.HasSuffix(path, ".else"):
			parentPath = trimSuffix(path, ".else")
		default:
			continue
		}
		properties.ForEach(
			func(key, value gjson.Result) bool {
				destination := joinPath(parentPath, "properties."+escapeGJSONPathKey(key.String()))
				if !gjson.Get(jsonStr, destination).Exists() {
					updated, _ := sjson.SetRawBytes([]byte(jsonStr), destination, []byte(value.Raw))
					jsonStr = string(updated)
				}
				return true
			},
		)
	}
	return jsonStr
}

// mergeObjectUnions retains an existing object schema and projects only missing
// branch properties into it. Alternative required lists are intentionally not
// promoted because no branch is unconditionally required.
func mergeObjectUnions(jsonStr string) string {
	for _, key := range []string{"anyOf", "oneOf"} {
		paths := findPaths(jsonStr, key)
		sortByDepth(paths)
		for _, path := range paths {
			parentPath := trimSuffix(path, "."+key)
			if isPropertyDefinition(parentPath) {
				continue
			}
			parent := gjson.Get(jsonStr, parentPath)
			if parentPath == "" {
				parent = gjson.Parse(jsonStr)
			}
			if !parent.Get("properties").IsObject() {
				continue
			}
			union := gjson.Get(jsonStr, path)
			if !union.IsArray() {
				continue
			}
			branches := union.Array()
			hasNull := false
			for _, branch := range branches {
				if branch.Get("type").String() == "null" {
					hasNull = true
				}
				properties := branch.Get("properties")
				if !properties.IsObject() {
					continue
				}
				properties.ForEach(
					func(propertyName, propertySchema gjson.Result) bool {
						destination := joinPath(
							parentPath,
							"properties."+escapeGJSONPathKey(propertyName.String()),
						)
						if !gjson.Get(jsonStr, destination).Exists() {
							updated, _ := sjson.SetRawBytes(
								[]byte(jsonStr),
								destination,
								[]byte(propertySchema.Raw),
							)
							jsonStr = string(updated)
						}
						return true
					},
				)
			}
			if hasNull {
				updated, _ := sjson.SetBytes([]byte(jsonStr), joinPath(parentPath, "nullable"), true)
				jsonStr = string(updated)
			}
			jsonStr, _ = sjson.Delete(jsonStr, path)
		}
	}
	return jsonStr
}

func mergeAllOf(jsonStr string) string {
	paths := findPaths(jsonStr, "allOf")
	sortByDepth(paths)

	for _, p := range paths {
		allOf := gjson.Get(jsonStr, p)
		if !allOf.IsArray() {
			continue
		}
		parentPath := trimSuffix(p, ".allOf")

		for _, item := range allOf.Array() {
			if props := item.Get("properties"); props.IsObject() {
				props.ForEach(
					func(key, value gjson.Result) bool {
						destPath := joinPath(parentPath, "properties."+escapeGJSONPathKey(key.String()))
						if !gjson.Get(jsonStr, destPath).Exists() {
							updated, _ := sjson.SetRawBytes([]byte(jsonStr), destPath, []byte(value.Raw))
							jsonStr = string(updated)
						}
						return true
					},
				)
			}
			if req := item.Get("required"); req.IsArray() {
				reqPath := joinPath(parentPath, "required")
				current := getStrings(jsonStr, reqPath)
				for _, r := range req.Array() {
					if s := r.String(); !contains(current, s) {
						current = append(current, s)
					}
				}
				updated, _ := sjson.SetBytes([]byte(jsonStr), reqPath, current)
				jsonStr = string(updated)
			}
		}
		jsonStr, _ = sjson.Delete(jsonStr, p)
	}
	return jsonStr
}

func flattenAnyOfOneOf(jsonStr string, flattenAll bool) string {
	for _, key := range []string{"anyOf", "oneOf"} {
		paths := findPaths(jsonStr, key)
		sortByDepth(paths)

		for _, p := range paths {
			arr := gjson.Get(jsonStr, p)
			if !arr.IsArray() || len(arr.Array()) == 0 {
				continue
			}

			parentPath := trimSuffix(p, "."+key)
			parentDesc := gjson.Get(jsonStr, descriptionPath(parentPath)).String()

			items := arr.Array()
			hasNull, nonNullCount, nonNullIndex := false, 0, -1
			for index, item := range items {
				if item.Get("type").String() == "null" {
					hasNull = true
				} else {
					nonNullCount++
					nonNullIndex = index
				}
			}
			if !flattenAll && (len(items) != 2 || !hasNull || nonNullCount != 1 ||
				items[nonNullIndex].Get("type").Type != gjson.String || items[nonNullIndex].Get("nullable").Bool()) {
				continue
			}
			bestIdx, allTypes := selectBest(items)
			if !flattenAll {
				bestIdx = nonNullIndex
			}
			selected := items[bestIdx].Raw
			if hasNull {
				selected, _ = sjson.Set(selected, "nullable", true)
			}

			if parentDesc != "" {
				selected = mergeDescriptionRaw(selected, parentDesc)
			}

			if len(allTypes) > 1 {
				hint := "Accepts: " + strings.Join(allTypes, " | ")
				selected = appendHintRaw(selected, hint)
			}

			jsonStr = setRawAt(jsonStr, parentPath, selected)
		}
	}
	return jsonStr
}

func selectBest(items []gjson.Result) (bestIdx int, types []string) {
	bestScore := -1
	for i, item := range items {
		t := item.Get("type").String()
		score := 0

		switch {
		case t == "object" || item.Get("properties").Exists():
			score, t = 3, orDefault(t, "object")
		case t == "array" || item.Get("items").Exists():
			score, t = 2, orDefault(t, "array")
		case t != "" && t != "null":
			score = 1
		default:
			t = orDefault(t, "null")
		}

		if t != "" {
			types = append(types, t)
		}
		if score > bestScore {
			bestScore, bestIdx = score, i
		}
	}
	return
}

func flattenTypeArrays(jsonStr string) string {
	paths := findPaths(jsonStr, "type")
	sortByDepth(paths)

	for _, p := range paths {
		res := gjson.Get(jsonStr, p)
		if !res.IsArray() || len(res.Array()) == 0 {
			continue
		}

		hasNull := false
		var nonNullTypes []string
		for _, item := range res.Array() {
			s := item.String()
			if s == "null" {
				hasNull = true
			} else if s != "" {
				nonNullTypes = append(nonNullTypes, s)
			}
		}

		parentPath := trimSuffix(p, ".type")
		firstType := "string"
		if len(nonNullTypes) > 0 {
			if gjson.Get(jsonStr, joinPath(parentPath, "items")).Exists() && contains(nonNullTypes, "array") {
				firstType = "array"
			} else {
				firstType = nonNullTypes[0]
			}
		}

		updated, _ := sjson.SetBytes([]byte(jsonStr), p, firstType)
		jsonStr = string(updated)
		if firstType != "array" && gjson.Get(jsonStr, joinPath(parentPath, "items")).Exists() {
			jsonStr, _ = sjson.Delete(jsonStr, joinPath(parentPath, "items"))
		}
		if len(nonNullTypes) > 1 {
			hint := "Accepts: " + strings.Join(nonNullTypes, " | ")
			jsonStr = appendHint(jsonStr, parentPath, hint)
		}

		if hasNull {
			updated, _ := sjson.SetBytes([]byte(jsonStr), joinPath(parentPath, "nullable"), true)
			jsonStr = string(updated)
			jsonStr = appendHint(jsonStr, parentPath, "(nullable)")
		}
	}

	return jsonStr
}

func removeUnsupportedKeywords(jsonStr string, options jsonSchemaCleanOptions) string {
	keywords := append([]string{}, unsupportedConstraints...)
	keywords = append(
		keywords,
		"$schema", "$defs", "definitions", "const", "$ref", "$id", "id", "additionalProperties",
		"$anchor", "$vocabulary", "$dynamicRef", "$dynamicAnchor",
		"propertyNames", "patternProperties", "additionalItems", "unevaluatedProperties", "unevaluatedItems",
		"contentSchema",
		"if", "then", "else",
		"$comment", "enumDescriptions", "enumTitles", "prefill", "encrypted",
		"deprecated",
	)

	deletePaths := make([]string, 0)
	pathsByField := findPathsByFields(jsonStr, keywords)
	for _, key := range keywords {
		for _, p := range pathsByField[key] {
			if isPropertyDefinition(trimSuffix(p, "."+key)) {
				continue
			}
			if key == "additionalProperties" && options.preserveAdditionalPropertiesFalse &&
				gjson.Get(jsonStr, p).Type == gjson.False {
				continue
			}
			deletePaths = append(deletePaths, p)
		}
	}
	sortByDepth(deletePaths)
	for _, p := range deletePaths {
		jsonStr, _ = sjson.Delete(jsonStr, p)
	}
	// Remove x-* extension fields (e.g., x-google-enum-descriptions) that are not supported by Gemini API
	jsonStr = removeExtensionFields(jsonStr)
	return jsonStr
}

// removeExtensionFields removes all x-* extension fields from the JSON schema.
// These are OpenAPI/JSON Schema extension fields that Google APIs don't recognize.
func removeExtensionFields(jsonStr string) string {
	var paths []string
	walkForExtensions(gjson.Parse(jsonStr), "", &paths)
	// walkForExtensions returns paths in a way that deeper paths are added before their ancestors
	// when they are not deleted wholesale, but since we skip children of deleted x-* nodes,
	// any collected path is safe to delete. We still use DeleteBytes for efficiency.

	b := []byte(jsonStr)
	for _, p := range paths {
		b, _ = sjson.DeleteBytes(b, p)
	}
	return string(b)
}

func walkForExtensions(value gjson.Result, path string, paths *[]string) {
	if value.IsArray() {
		arr := value.Array()
		for i, a := range slices.Backward(arr) {
			walkForExtensions(a, joinPath(path, strconv.Itoa(i)), paths)
		}
		return
	}

	if value.IsObject() {
		value.ForEach(
			func(key, val gjson.Result) bool {
				keyStr := key.String()
				safeKey := escapeGJSONPathKey(keyStr)
				childPath := joinPath(path, safeKey)

				// If it's an extension field, we delete it and don't need to look at its children.
				if strings.HasPrefix(keyStr, "x-") && !isPropertyDefinition(path) {
					*paths = append(*paths, childPath)
					return true
				}

				walkForExtensions(val, childPath, paths)
				return true
			},
		)
	}
}

func cleanupRequiredFields(jsonStr string) string {
	for _, p := range findPaths(jsonStr, "required") {
		parentPath := trimSuffix(p, ".required")
		propsPath := joinPath(parentPath, "properties")

		req := gjson.Get(jsonStr, p)
		props := gjson.Get(jsonStr, propsPath)
		if !req.IsArray() {
			continue
		}
		if !props.IsObject() {
			jsonStr, _ = sjson.Delete(jsonStr, p)
			continue
		}

		var valid []string
		for _, r := range req.Array() {
			key := r.String()
			if props.Get(escapeGJSONPathKey(key)).Exists() {
				valid = append(valid, key)
			}
		}

		if len(valid) != len(req.Array()) {
			if len(valid) == 0 {
				jsonStr, _ = sjson.Delete(jsonStr, p)
			} else {
				updated, _ := sjson.SetBytes([]byte(jsonStr), p, valid)
				jsonStr = string(updated)
			}
		}
	}
	return jsonStr
}

// addEmptySchemaPlaceholder adds a placeholder "reason" property to empty object schemas.
// Claude VALIDATED mode requires at least one required property in tool schemas.
func addEmptySchemaPlaceholder(jsonStr string) string {
	// Find all "type" fields
	paths := findPaths(jsonStr, "type")

	// Process from deepest to shallowest (to handle nested objects properly)
	sortByDepth(paths)

	for _, p := range paths {
		typeVal := gjson.Get(jsonStr, p)
		if typeVal.String() != "object" {
			continue
		}

		// Get the parent path (the object containing "type")
		parentPath := trimSuffix(p, ".type")

		// Check if properties exists and is empty or missing
		propsPath := joinPath(parentPath, "properties")
		propsVal := gjson.Get(jsonStr, propsPath)
		reqPath := joinPath(parentPath, "required")
		reqVal := gjson.Get(jsonStr, reqPath)
		hasRequiredProperties := reqVal.IsArray() && len(reqVal.Array()) > 0

		needsPlaceholder := false
		if !propsVal.Exists() {
			// No properties field at all
			needsPlaceholder = true
		} else if propsVal.IsObject() && len(propsVal.Map()) == 0 {
			// Empty properties object
			needsPlaceholder = true
		}

		if needsPlaceholder {
			// Add placeholder "reason" property
			reasonPath := joinPath(propsPath, "reason")
			updated, _ := sjson.SetBytes([]byte(jsonStr), reasonPath+".type", "string")
			jsonStr = string(updated)
			updated, _ = sjson.SetBytes(
				[]byte(jsonStr),
				reasonPath+".description",
				placeholderReasonDescription,
			)
			jsonStr = string(updated)

			// Add to required array
			updated, _ = sjson.SetBytes([]byte(jsonStr), reqPath, []string{"reason"})
			jsonStr = string(updated)
			continue
		}

		// If schema has properties but none are required, add a minimal placeholder.
		if propsVal.IsObject() && !hasRequiredProperties {
			// DO NOT add placeholder if it's a top-level schema (parentPath is empty)
			// or if we've already added a placeholder reason above.
			if parentPath == "" {
				continue
			}
			placeholderPath := joinPath(propsPath, "_")
			if !gjson.Get(jsonStr, placeholderPath).Exists() {
				updated, _ := sjson.SetBytes([]byte(jsonStr), placeholderPath+".type", "boolean")
				jsonStr = string(updated)
			}
			updated, _ := sjson.SetBytes([]byte(jsonStr), reqPath, []string{"_"})
			jsonStr = string(updated)
		}
	}

	return jsonStr
}

// --- Helpers ---

func findPaths(jsonStr, field string) []string {
	var paths []string
	Walk(gjson.Parse(jsonStr), "", field, &paths)
	return paths
}

func findPathsByFields(jsonStr string, fields []string) map[string][]string {
	set := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		set[field] = struct{}{}
	}
	paths := make(map[string][]string, len(set))
	walkForFields(gjson.Parse(jsonStr), "", set, paths)
	return paths
}

func walkForFields(value gjson.Result, path string, fields map[string]struct{}, paths map[string][]string) {
	switch value.Type {
	case gjson.JSON:
		value.ForEach(
			func(key, val gjson.Result) bool {
				keyStr := key.String()
				safeKey := escapeGJSONPathKey(keyStr)

				var childPath string
				if path == "" {
					childPath = safeKey
				} else {
					childPath = path + "." + safeKey
				}

				if _, ok := fields[keyStr]; ok {
					paths[keyStr] = append(paths[keyStr], childPath)
				}

				walkForFields(val, childPath, fields, paths)
				return true
			},
		)
	case gjson.String, gjson.Number, gjson.True, gjson.False, gjson.Null:
		// Terminal types - no further traversal needed
	}
}

func sortByDepth(paths []string) {
	sort.Slice(paths, func(i, j int) bool { return len(paths[i]) > len(paths[j]) })
}

func trimSuffix(path, suffix string) string {
	if path == strings.TrimPrefix(suffix, ".") {
		return ""
	}
	return strings.TrimSuffix(path, suffix)
}

func joinPath(base, suffix string) string {
	if base == "" {
		return suffix
	}
	return base + "." + suffix
}

func setRawAt(jsonStr, path, value string) string {
	if path == "" {
		return value
	}
	result, _ := sjson.SetRawBytes([]byte(jsonStr), path, []byte(value))
	return string(result)
}

// schemaNameMapKeywords map author-chosen names to subschemas.
var schemaNameMapKeywords = map[string]struct{}{
	"properties":        {},
	"patternProperties": {},
	"dependentSchemas":  {},
	"$defs":             {},
	"definitions":       {},
}

// isPropertyDefinition uses the trailing name-map run parity so a property named
// "properties" is classified as a schema node rather than another name map.
func isPropertyDefinition(path string) bool {
	segments := splitGJSONPath(path)
	trailing := 0
	for _, segment := range slices.Backward(segments) {
		if _, ok := schemaNameMapKeywords[unescapeGJSONPathKey(segment)]; !ok {
			break
		}
		trailing++
	}
	return trailing%2 == 1
}

func descriptionPath(parentPath string) string {
	if parentPath == "" || parentPath == "@this" {
		return "description"
	}
	return parentPath + ".description"
}

// mergeHint combines an existing description with a hint without duplicating an earlier pass.
func mergeHint(existing, hint string) string {
	if existing == "" {
		return hint
	}
	if existing == hint ||
		strings.HasPrefix(existing, hint+" (") ||
		strings.Contains(existing, fmt.Sprintf("(%s)", hint)) {
		return existing
	}
	return fmt.Sprintf("%s (%s)", existing, hint)
}

func appendHint(jsonStr, parentPath, hint string) string {
	descPath := parentPath + ".description"
	if parentPath == "" || parentPath == "@this" {
		descPath = "description"
	}
	merged := mergeHint(gjson.Get(jsonStr, descPath).String(), hint)
	updated, _ := sjson.SetBytes([]byte(jsonStr), descPath, merged)
	jsonStr = string(updated)
	return jsonStr
}

func appendHintRaw(jsonRaw, hint string) string {
	merged := mergeHint(gjson.Get(jsonRaw, "description").String(), hint)
	updated, _ := sjson.SetBytes([]byte(jsonRaw), "description", merged)
	jsonRaw = string(updated)
	return jsonRaw
}

func getStrings(jsonStr, path string) []string {
	var result []string
	if arr := gjson.Get(jsonStr, path); arr.IsArray() {
		for _, r := range arr.Array() {
			result = append(result, r.String())
		}
	}
	return result
}

func contains(slice []string, item string) bool {
	return slices.Contains(slice, item)
}

func orDefault(val, def string) string {
	if val == "" {
		return def
	}
	return val
}

func escapeGJSONPathKey(key string) string {
	if !strings.ContainsAny(key, ".*?") {
		return key
	}
	return gjsonPathKeyReplacer.Replace(key)
}

func unescapeGJSONPathKey(key string) string {
	if !strings.Contains(key, "\\") {
		return key
	}
	var b strings.Builder
	b.Grow(len(key))
	for i := 0; i < len(key); i++ {
		if key[i] == '\\' && i+1 < len(key) {
			i++
			b.WriteByte(key[i])
			continue
		}
		b.WriteByte(key[i])
	}
	return b.String()
}

func splitGJSONPath(path string) []string {
	if path == "" {
		return nil
	}

	parts := make([]string, 0, strings.Count(path, ".")+1)
	var b strings.Builder
	b.Grow(len(path))

	for i := 0; i < len(path); i++ {
		c := path[i]
		if c == '\\' && i+1 < len(path) {
			b.WriteByte('\\')
			i++
			b.WriteByte(path[i])
			continue
		}
		if c == '.' {
			parts = append(parts, b.String())
			b.Reset()
			continue
		}
		b.WriteByte(c)
	}
	parts = append(parts, b.String())
	return parts
}

func mergeDescriptionRaw(schemaRaw, parentDesc string) string {
	childDesc := gjson.Get(schemaRaw, "description").String()
	switch {
	case childDesc == "":
		updated, _ := sjson.SetBytes([]byte(schemaRaw), "description", parentDesc)
		return string(updated)
	case childDesc == parentDesc:
		return schemaRaw
	default:
		combined := fmt.Sprintf("%s (%s)", parentDesc, childDesc)
		updated, _ := sjson.SetBytes([]byte(schemaRaw), "description", combined)
		return string(updated)
	}
}
