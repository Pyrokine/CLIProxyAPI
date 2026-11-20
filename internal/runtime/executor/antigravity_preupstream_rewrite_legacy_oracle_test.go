package executor

// 固定批量改写前的工具响应与 Schema 算法，供差分回归逐字节比较
// 这些旧算法保持独立，不调用生产改写函数

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func legacyAntigravityCountClaudeToolProvenanceIDs(payload []byte) int {
	count := 0
	contents := util.GetGJSONBytesNoCopy(payload, "request.contents")
	if !contents.IsArray() {
		return 0
	}
	for _, content := range contents.Array() {
		for _, part := range content.Get("parts").Array() {
			for _, path := range []string{"functionCall.id", "functionResponse.id"} {
				if util.IsGeminiClaudeToolUseID(part.Get(path).String()) {
					count++
				}
			}
		}
	}
	return count
}

func legacyAntigravityPayloadHasClaudeToolProvenanceID(payload []byte) bool {
	contents := util.GetGJSONBytesNoCopy(payload, "request.contents")
	if !contents.IsArray() {
		return false
	}
	for _, content := range contents.Array() {
		for _, part := range content.Get("parts").Array() {
			for _, path := range []string{"functionCall.id", "functionResponse.id"} {
				if util.IsGeminiClaudeToolUseID(part.Get(path).String()) {
					return true
				}
			}
		}
	}
	return false
}

func legacyNormalizeAntigravityGeminiFunctionResponseRoles(rawJSON []byte) []byte {
	rawJSON = legacyRepairAntigravityGeminiFunctionResponseNames(rawJSON)
	contents := util.GetGJSONBytesNoCopy(rawJSON, "request.contents")
	if !contents.IsArray() {
		return rawJSON
	}
	type functionRef struct {
		id   string
		name string
	}
	out := rawJSON
	var pending []functionRef
	for contentIndex, content := range contents.Array() {
		parts := content.Get("parts")
		if !parts.IsArray() || len(parts.Array()) == 0 {
			pending = nil
			continue
		}
		var calls, responses []functionRef
		var responseParts, otherParts []json.RawMessage
		hasOtherPart := false
		parts.ForEach(
			func(_, part gjson.Result) bool {
				switch {
				case part.Get("functionCall").Exists():
					calls = append(
						calls, functionRef{
							id: part.Get("functionCall.id").String(), name: part.Get("functionCall.name").String(),
						},
					)
				case part.Get("functionResponse").Exists():
					responses = append(
						responses, functionRef{
							id:   part.Get("functionResponse.id").String(),
							name: part.Get("functionResponse.name").String(),
						},
					)
					responseParts = append(responseParts, json.RawMessage(part.Raw))
				default:
					hasOtherPart = true
					otherParts = append(otherParts, json.RawMessage(part.Raw))
				}
				return true
			},
		)
		if len(calls) > 0 && len(responses) == 0 {
			pending = calls
			continue
		}
		if len(responses) == 0 {
			if hasOtherPart {
				pending = nil
			}
			continue
		}
		if len(calls) > 0 {
			pending = nil
			continue
		}

		if len(pending) > 0 && len(responses) > 0 {
			ordered := make([]json.RawMessage, 0, len(responseParts))
			used := make([]bool, len(responses))
			for _, call := range pending {
				for responseIndex, response := range responses {
					if used[responseIndex] {
						continue
					}
					if (call.id != "" && response.id == call.id) ||
						(call.id == "" && call.name != "" && response.name == call.name) {
						used[responseIndex] = true
						ordered = append(ordered, responseParts[responseIndex])
						break
					}
				}
			}
			for responseIndex := range responses {
				if !used[responseIndex] {
					ordered = append(ordered, responseParts[responseIndex])
				}
			}
			if len(ordered) == len(responseParts) {
				ordered = append(ordered, otherParts...)
				if encoded, errMarshal := json.Marshal(ordered); errMarshal == nil {
					if updated, errSet := sjson.SetRawBytes(
						out, fmt.Sprintf("request.contents.%d.parts", contentIndex), encoded,
					); errSet == nil {
						out = updated
					}
				}
			}
		}
		pending = nil
		if !hasOtherPart && content.Get("role").String() != "model" {
			if updated, errSet := sjson.SetBytes(
				out, fmt.Sprintf("request.contents.%d.role", contentIndex), "model",
			); errSet == nil {
				out = updated
			}
		}
	}
	return out
}

func legacyRepairAntigravityGeminiFunctionResponseNames(rawJSON []byte) []byte {
	contents := util.GetGJSONBytesNoCopy(rawJSON, "request.contents")
	if !contents.IsArray() {
		return rawJSON
	}
	callIDToName := make(map[string]string)
	contents.ForEach(
		func(_, content gjson.Result) bool {
			parts := content.Get("parts")
			if !parts.IsArray() {
				return true
			}
			parts.ForEach(
				func(_, part gjson.Result) bool {
					fc := part.Get("functionCall")
					if fc.Exists() {
						id := strings.TrimSpace(fc.Get("id").String())
						name := strings.TrimSpace(fc.Get("name").String())
						if id != "" && name != "" && name != "unknown" {
							callIDToName[id] = name
						}
					}
					return true
				},
			)
			return true
		},
	)
	if len(callIDToName) == 0 {
		return rawJSON
	}

	out := rawJSON
	contents.ForEach(
		func(contentIdx, content gjson.Result) bool {
			parts := content.Get("parts")
			if !parts.IsArray() {
				return true
			}
			parts.ForEach(
				func(partIdx, part gjson.Result) bool {
					fr := part.Get("functionResponse")
					if fr.Exists() {
						id := strings.TrimSpace(fr.Get("id").String())
						name := strings.TrimSpace(fr.Get("name").String())
						if id != "" && (name == "" || name == "unknown") {
							if realName, ok := callIDToName[id]; ok {
								path := fmt.Sprintf(
									"request.contents.%d.parts.%d.functionResponse.name", contentIdx.Int(),
									partIdx.Int(),
								)
								if updated, errSet := sjson.SetBytes(out, path, realName); errSet == nil {
									out = updated
								}
							}
						}
					}
					return true
				},
			)
			return true
		},
	)
	return out
}

func legacySanitizeAntigravityRequestSchemas(payloadStr string, useAntigravitySchema bool) string {
	for _, base := range antigravityFunctionDeclarationPaths(payloadStr) {
		oldPath := base + ".parametersJsonSchema"
		if !gjson.Get(payloadStr, oldPath).Exists() {
			continue
		}
		renamed, errRename := util.RenameKey(payloadStr, oldPath, base+".parameters")
		if errRename != nil {
			log.Debugf("antigravity: failed to rename %s: %v", oldPath, errRename)
			continue
		}
		payloadStr = renamed
	}

	toolSchemaCleaner := util.CleanJSONSchemaForGemini
	if useAntigravitySchema {
		toolSchemaCleaner = util.CleanJSONSchemaForAntigravity
	}
	responseSchemaCleaner := util.CleanJSONSchemaForAntigravityResponse
	cleanNestedToolSchema := func(schemaRaw string) string {
		return legacyCleanNestedAntigravitySchema(toolSchemaCleaner, schemaRaw)
	}
	payloadStr = legacyCleanAntigravitySchemasAtPaths(
		payloadStr,
		antigravityDeclarationSchemaPaths(payloadStr),
		cleanNestedToolSchema,
	)
	return legacyCleanAntigravitySchemasAtPaths(
		payloadStr,
		legacyAntigravityGenerationSchemaPaths(payloadStr),
		responseSchemaCleaner,
	)
}

func legacyCleanAntigravitySchemasAtPaths(payloadStr string, schemaPaths []string, clean func(string) string) string {
	for _, schemaPath := range schemaPaths {
		schema := gjson.Get(payloadStr, schemaPath)
		if !schema.Exists() {
			continue
		}
		updated, errSet := sjson.SetRawBytes([]byte(payloadStr), schemaPath, []byte(clean(schema.Raw)))
		if errSet != nil {
			continue
		}
		payloadStr = string(updated)
	}
	return payloadStr
}

func legacyAntigravityGenerationSchemaPaths(payloadStr string) []string {
	paths := make([]string, 0, 8)
	for _, container := range []string{"request.generationConfig", "request.generation_config"} {
		for _, key := range []string{
			"responseSchema", "responseJsonSchema", "response_schema", "response_json_schema",
		} {
			path := container + "." + key
			if gjson.Get(payloadStr, path).IsObject() {
				paths = append(paths, path)
			}
		}
	}
	return paths
}

func legacyCleanNestedAntigravitySchema(clean func(string) string, schemaRaw string) string {
	wrapped, err := sjson.SetRaw("{}", "schema", schemaRaw)
	if err != nil {
		return clean(schemaRaw)
	}
	if unwrapped := gjson.Get(clean(wrapped), "schema"); unwrapped.Exists() {
		return unwrapped.Raw
	}
	return clean(schemaRaw)
}

// 按已采用的字段含义逐项改写完整正文，独立比较批量替换的输出
func perFieldAntigravityRequestSchemas(payload string, useAntigravitySchema bool) string {
	toolCleaner := util.CleanJSONSchemaForGemini
	nativeCleaner := util.CleanJSONSchemaForGeminiJSONSchema
	if useAntigravitySchema {
		toolCleaner = util.CleanJSONSchemaForAntigravity
		nativeCleaner = util.CleanJSONSchemaForAntigravityJSONSchema
	}
	fields := []struct {
		key   string
		clean func(string) string
	}{
		{"parameters", toolCleaner},
		{"parametersJsonSchema", nativeCleaner},
		{"parameters_json_schema", nativeCleaner},
		{"response", util.CleanJSONSchemaForAntigravityResponse},
		{"responseJsonSchema", util.CleanJSONSchemaForGeminiJSONSchema},
		{"response_json_schema", util.CleanJSONSchemaForGeminiJSONSchema},
	}
	if tools := gjson.Get(payload, "request.tools"); tools.IsArray() {
		for toolIndex, tool := range tools.Array() {
			for _, key := range []string{"functionDeclarations", "function_declarations"} {
				declarations := tool.Get(key)
				if !declarations.IsArray() {
					continue
				}
				for index, declaration := range declarations.Array() {
					for _, field := range fields {
						schema := declaration.Get(field.key)
						if !schema.IsObject() {
							continue
						}
						cleaned := legacyCleanNestedAntigravitySchema(field.clean, schema.Raw)
						if cleaned == schema.Raw {
							continue
						}
						path := fmt.Sprintf("request.tools.%d.%s.%d.%s", toolIndex, key, index, field.key)
						if updated, err := sjson.SetRaw(payload, path, cleaned); err == nil {
							payload = updated
						}
					}
				}
			}
		}
	}
	for _, container := range []string{"request.generationConfig", "request.generation_config"} {
		generation := gjson.Get(payload, container)
		if !generation.IsObject() {
			continue
		}
		for _, key := range []string{
			"responseSchema", "responseJsonSchema", "response_schema", "response_json_schema",
		} {
			schema := generation.Get(key)
			if !schema.IsObject() {
				continue
			}
			clean := util.CleanJSONSchemaForAntigravityResponse
			if key == "responseJsonSchema" || key == "response_json_schema" {
				clean = util.CleanJSONSchemaForGeminiJSONSchema
			}
			if cleaned := clean(schema.Raw); cleaned != schema.Raw {
				if updated, err := sjson.SetRaw(payload, container+"."+key, cleaned); err == nil {
					payload = updated
				}
			}
		}
	}
	return payload
}
