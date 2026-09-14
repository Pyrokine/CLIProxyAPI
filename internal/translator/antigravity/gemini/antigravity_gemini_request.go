// Package gemini provides request translation functionality for Gemini CLI to Gemini API compatibility.
// It handles parsing and transforming Gemini CLI API requests into Gemini API format,
// extracting model information, system instructions, message contents, and tool declarations.
// The package performs JSON data transformation to ensure compatibility
// between Gemini CLI API format and Gemini API's expected format.
package gemini

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/signature"
	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/gemini/common"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ConvertGeminiRequestToAntigravity parses and transforms a Gemini CLI API request into Gemini API format.
// It extracts the model name, system instruction, message contents, and tool declarations
// from the raw JSON request and returns them in the format expected by the Gemini API.
// The function performs the following transformations:
// 1. Extracts the model information from the request
// 2. Restructures the JSON to match Gemini API format
// 3. Converts system instructions to the expected format
// 4. Fixes CLI tool response format and grouping
//
// Parameters:
//   - modelName: The name of the model to use for the request (unused in current implementation)
//   - rawJSON: The raw JSON request data from the Gemini CLI API
//   - stream: A boolean indicating if the request is for a streaming response (unused in current implementation)
//
// Returns:
//   - []byte: The transformed request data in Gemini API format
func ConvertGeminiRequestToAntigravity(modelName string, inputRawJSON []byte, _ bool) []byte {
	rawJSON := inputRawJSON
	functionNameMap := util.SanitizedFunctionNameMap(inputRawJSON)
	// Keep the envelope in []byte form. Round-tripping through string copies the
	// entire request, which dominates allocations for large inline data. Fill the
	// small envelope fields first so the payload is only spliced in once.
	envelope, _ := sjson.SetBytes([]byte(`{"project":"","request":{},"model":""}`), "model", modelName)
	rawJSON, _ = sjson.SetRawBytes(envelope, "request", rawJSON)
	if util.GetGJSONBytesNoCopy(rawJSON, "request.model").Exists() {
		rawJSON, _ = sjson.DeleteBytes(rawJSON, "request.model")
	}

	fixedJSON, errFixCLIToolResponse := fixCLIToolResponse(rawJSON)
	if errFixCLIToolResponse != nil {
		return []byte{}
	}
	rawJSON = fixedJSON

	if systemInstructionResult := util.GetGJSONBytesNoCopy(
		rawJSON, "request.system_instruction",
	); systemInstructionResult.Exists() {
		rawJSON, _ = sjson.SetRawBytes(
			rawJSON,
			"request.systemInstruction",
			[]byte(systemInstructionResult.Raw),
		)
		rawJSON, _ = sjson.DeleteBytes(rawJSON, "request.system_instruction")
	}

	// Normalize roles in request.contents: default to valid values if missing/invalid.
	// The contents array is only materialized when a role actually changes; copying
	// every content up front duplicates the whole payload for large inline data.
	contents := util.GetGJSONBytesNoCopy(rawJSON, "request.contents")
	if contents.IsArray() && geminiContentRolesNeedNormalization(contents) {
		contentItems := translatorcommon.NewRawArrayItems(contents.Get("#").Int())
		previousRole := ""
		contents.ForEach(
			func(_, value gjson.Result) bool {
				role := value.Get("role").String()
				content := []byte(value.Raw)
				if role != "user" && role != "model" {
					if previousRole == "" || previousRole == "model" {
						role = "user"
					} else {
						role = "model"
					}
					content, _ = sjson.SetBytes(content, "role", role)
				}
				previousRole = role
				contentItems = append(contentItems, content)
				return true
			},
		)
		rawJSON, _ = sjson.SetRawBytes(
			rawJSON,
			"request.contents",
			translatorcommon.JoinRawArray(contentItems),
		)
	}

	toolsResult := util.GetGJSONBytesNoCopy(rawJSON, "request.tools")
	if toolsResult.IsArray() {
		seenFunctionNames := make(map[string]struct{})
		toolsChanged := false
		var toolItems [][]byte
		toolsResult.ForEach(
			func(toolIndex, tool gjson.Result) bool {
				toolJSON := []byte(tool.Raw)
				toolChanged := false
				for _, key := range []string{"functionDeclarations", "function_declarations"} {
					declarations := tool.Get(key)
					if !declarations.IsArray() {
						continue
					}

					declarationsChanged := false
					var declarationItems [][]byte
					declarations.ForEach(
						func(_, declaration gjson.Result) bool {
							nameResult := declaration.Get("name")
							originalName := nameResult.String()
							mappedName := util.MapSanitizedFunctionName(functionNameMap, originalName)
							if mappedName != "" {
								if _, exists := seenFunctionNames[mappedName]; exists {
									declarationsChanged = true
									return true
								}
								seenFunctionNames[mappedName] = struct{}{}
							}

							declarationJSON := []byte(declaration.Raw)
							if nameResult.Type != gjson.String || mappedName != originalName {
								declarationJSON, _ = sjson.SetBytes(declarationJSON, "name", mappedName)
								declarationsChanged = true
							}
							if parameters := declaration.Get("parameters"); parameters.Exists() {
								declarationJSON, _ = sjson.SetRawBytes(
									declarationJSON,
									"parametersJsonSchema",
									[]byte(parameters.Raw),
								)
								declarationJSON, _ = sjson.DeleteBytes(declarationJSON, "parameters")
								declarationsChanged = true
							}
							declarationItems = append(declarationItems, declarationJSON)
							return true
						},
					)
					if declarationsChanged {
						var errSet error
						toolJSON, errSet = sjson.SetRawBytes(
							toolJSON,
							key,
							translatorcommon.JoinRawArray(declarationItems),
						)
						if errSet != nil {
							log.Warnf(
								"failed to normalize function declarations in tool %d: %v", toolIndex.Int(), errSet,
							)
						} else {
							toolChanged = true
						}
					}
				}
				toolsChanged = toolsChanged || toolChanged
				toolItems = append(toolItems, toolJSON)
				return true
			},
		)
		if toolsChanged {
			rawJSON, _ = sjson.SetRawBytes(
				rawJSON,
				"request.tools",
				translatorcommon.JoinRawArray(toolItems),
			)
		}
		rawJSON = removeEmptyGeminiFunctionTools(rawJSON)
	}
	rawJSON = rewriteGeminiFunctionNames(rawJSON, functionNameMap)

	rawJSON = sanitizeAntigravityGeminiSignaturesForModel(rawJSON, modelName)

	return common.AttachDefaultSafetySettings(rawJSON, "request.safetySettings")
}

func geminiContentRolesNeedNormalization(contents gjson.Result) bool {
	needsNormalization := false
	contents.ForEach(
		func(_, value gjson.Result) bool {
			role := value.Get("role").String()
			if role != "user" && role != "model" {
				needsNormalization = true
				return false
			}
			return true
		},
	)
	return needsNormalization
}

func removeEmptyGeminiFunctionTools(rawJSON []byte) []byte {
	tools := util.GetGJSONBytesNoCopy(rawJSON, "request.tools")
	if tools.IsArray() && len(tools.Array()) == 0 {
		rawJSON, _ = sjson.DeleteBytes(rawJSON, "request.tools")
		return rawJSON
	}
	changed := false
	var cleanedTools [][]byte
	for _, tool := range tools.Array() {
		toolJSON := []byte(tool.Raw)
		if tool.IsObject() {
			for _, key := range []string{"functionDeclarations", "function_declarations"} {
				if declarations := tool.Get(key); declarations.IsArray() && len(declarations.Array()) == 0 {
					toolJSON, _ = sjson.DeleteBytes(toolJSON, key)
					changed = true
				}
			}
			if len(util.ParseGJSONBytesNoCopy(toolJSON).Map()) == 0 {
				changed = true
				continue
			}
		}
		cleanedTools = append(cleanedTools, toolJSON)
	}
	if !changed {
		return rawJSON
	}
	if len(cleanedTools) == 0 {
		rawJSON, _ = sjson.DeleteBytes(rawJSON, "request.tools")
		return rawJSON
	}
	rawJSON, _ = sjson.SetRawBytes(
		rawJSON,
		"request.tools",
		translatorcommon.JoinRawArray(cleanedTools),
	)
	return rawJSON
}

var geminiFunctionNameFields = []string{
	"functionCall",
	"functionResponse",
	"function_call",
	"function_response",
}

func geminiFunctionNamesNeedRewrite(contents gjson.Result, functionNameMap map[string]string) bool {
	needsRewrite := false
	contents.ForEach(
		func(_, content gjson.Result) bool {
			content.Get("parts").ForEach(
				func(_, part gjson.Result) bool {
					for _, field := range geminiFunctionNameFields {
						nameResult := part.Get(field + ".name")
						name := nameResult.String()
						if name == "" {
							continue
						}
						if nameResult.Type == gjson.String && util.MapSanitizedFunctionName(
							functionNameMap, name,
						) == name {
							continue
						}
						needsRewrite = true
						return false
					}
					return true
				},
			)
			return !needsRewrite
		},
	)
	return needsRewrite
}

func rewriteGeminiFunctionNames(rawJSON []byte, functionNameMap map[string]string) []byte {
	contents := util.GetGJSONBytesNoCopy(rawJSON, "request.contents")
	canBatchContents := contents.IsArray()
	if canBatchContents {
		contents.ForEach(
			func(_, content gjson.Result) bool {
				parts := content.Get("parts")
				if parts.Exists() && !parts.IsArray() {
					canBatchContents = false
					return false
				}
				return true
			},
		)
	}
	if canBatchContents && geminiFunctionNamesNeedRewrite(contents, functionNameMap) {
		contentItems := translatorcommon.NewRawArrayItems(contents.Get("#").Int())
		contents.ForEach(
			func(_, content gjson.Result) bool {
				contentJSON := []byte(content.Raw)
				partsChanged := false
				partItems := make([][]byte, 0, 4)
				content.Get("parts").ForEach(
					func(_, part gjson.Result) bool {
						partJSON := []byte(part.Raw)
						for _, field := range geminiFunctionNameFields {
							nameResult := part.Get(field + ".name")
							name := nameResult.String()
							if name == "" {
								continue
							}
							mappedName := util.MapSanitizedFunctionName(functionNameMap, name)
							if nameResult.Type == gjson.String && mappedName == name {
								continue
							}
							partJSON, _ = sjson.SetBytes(partJSON, field+".name", mappedName)
							partsChanged = true
						}
						partItems = append(partItems, partJSON)
						return true
					},
				)
				if partsChanged {
					contentJSON, _ = sjson.SetRawBytes(
						contentJSON,
						"parts",
						translatorcommon.JoinRawArray(partItems),
					)
				}
				contentItems = append(contentItems, contentJSON)
				return true
			},
		)
		rawJSON, _ = sjson.SetRawBytes(
			rawJSON,
			"request.contents",
			translatorcommon.JoinRawArray(contentItems),
		)
	} else if !canBatchContents {
		for contentIndex, content := range contents.Array() {
			for partIndex, part := range content.Get("parts").Array() {
				for _, field := range geminiFunctionNameFields {
					nameResult := part.Get(field + ".name")
					name := nameResult.String()
					if name == "" {
						continue
					}
					mappedName := util.MapSanitizedFunctionName(functionNameMap, name)
					if nameResult.Type == gjson.String && mappedName == name {
						continue
					}
					path := fmt.Sprintf(
						"request.contents.%d.parts.%d.%s.name",
						contentIndex,
						partIndex,
						field,
					)
					rawJSON, _ = sjson.SetBytes(rawJSON, path, mappedName)
				}
			}
		}
	}

	for _, allowedPath := range []string{
		"request.toolConfig.functionCallingConfig.allowedFunctionNames",
		"request.tool_config.function_calling_config.allowed_function_names",
	} {
		allowedNames := util.GetGJSONBytesNoCopy(rawJSON, allowedPath)
		if !allowedNames.IsArray() {
			continue
		}
		namesChanged := false
		nameItems := make([][]byte, 0, len(allowedNames.Array()))
		allowedNames.ForEach(
			func(_, name gjson.Result) bool {
				mappedName := util.MapSanitizedFunctionName(functionNameMap, name.String())
				namesChanged = namesChanged || name.Type != gjson.String || mappedName != name.String()
				mappedNameJSON, _ := json.Marshal(mappedName)
				nameItems = append(nameItems, mappedNameJSON)
				return true
			},
		)
		if namesChanged {
			rawJSON, _ = sjson.SetRawBytes(
				rawJSON,
				allowedPath,
				translatorcommon.JoinRawArray(nameItems),
			)
		}
	}
	return rawJSON
}

func sanitizeAntigravityGeminiSignaturesForModel(rawJSON []byte, modelName string) []byte {
	switch signature.ProviderFromModelName(modelName) {
	case signature.ProviderClaude:
		return SanitizeAntigravityClaudeGeminiRequestSignatures(modelName, rawJSON)
	default:
		return signature.SanitizeGeminiRequestThoughtSignatures(rawJSON, "request.contents")
	}
}

func SanitizeAntigravityClaudeGeminiRequestSignatures(modelName string, rawJSON []byte) []byte {
	if signature.ProviderFromModelName(modelName) != signature.ProviderClaude {
		return rawJSON
	}
	contents := util.GetGJSONBytesNoCopy(rawJSON, "request.contents")
	if !contents.IsArray() {
		return rawJSON
	}

	contentsChanged := false
	contentItems := translatorcommon.NewRawArrayItems(contents.Get("#").Int())
	contents.ForEach(
		func(_, content gjson.Result) bool {
			parts := content.Get("parts")
			if !parts.IsArray() {
				contentItems = append(contentItems, []byte(content.Raw))
				return true
			}

			isModelTurn := content.Get("role").String() == "model"
			partsChanged := false
			partItems := translatorcommon.NewRawArrayItems(parts.Get("#").Int())
			parts.ForEach(
				func(_, part gjson.Result) bool {
					partJSON := []byte(part.Raw)
					rawSignature, hasSignature := antigravityGeminiSignatureField(part)
					if part.Get("functionResponse").Exists() || part.Get("function_response").Exists() || !isModelTurn {
						if hasSignature {
							partJSON = deleteAntigravityGeminiSignatureFields(partJSON)
							partsChanged = true
						}
						partItems = append(partItems, partJSON)
						return true
					}

					if part.Get("thought").Bool() {
						normalized, compatible := signature.CompatibleAntigravityClaudeThinkingSignature(rawSignature)
						if !compatible || strings.TrimSpace(part.Get("text").String()) == "" {
							partsChanged = true
							return true
						}
						if !hasNormalizedAntigravityGeminiSignature(part, normalized) {
							partJSON = deleteAntigravityGeminiSignatureFields(partJSON)
							partJSON, _ = sjson.SetBytes(partJSON, "thoughtSignature", normalized)
							partsChanged = true
						}
						partItems = append(partItems, partJSON)
						return true
					}

					if hasSignature {
						partJSON = deleteAntigravityGeminiSignatureFields(partJSON)
						partsChanged = true
					}
					if hasAntigravityGeminiContentPart(gjson.ParseBytes(partJSON)) {
						partItems = append(partItems, partJSON)
						return true
					}
					partsChanged = true
					return true
				},
			)

			if len(partItems) == 0 {
				contentsChanged = true
				return true
			}
			contentJSON := []byte(content.Raw)
			if partsChanged {
				contentJSON, _ = sjson.SetRawBytes(contentJSON, "parts", translatorcommon.JoinRawArray(partItems))
				contentsChanged = true
			}
			contentItems = append(contentItems, contentJSON)
			return true
		},
	)

	if !contentsChanged {
		return rawJSON
	}
	updated, errSet := sjson.SetRawBytes(rawJSON, "request.contents", translatorcommon.JoinRawArray(contentItems))
	if errSet != nil {
		return rawJSON
	}
	return updated
}

var antigravityGeminiSignaturePaths = []string{
	"thoughtSignature",
	"thought_signature",
	"functionCall.thoughtSignature",
	"functionCall.thought_signature",
	"functionResponse.thoughtSignature",
	"functionResponse.thought_signature",
	"extra_content.google.thought_signature",
}

func antigravityGeminiSignatureField(part gjson.Result) (string, bool) {
	for _, path := range antigravityGeminiSignaturePaths {
		value := part.Get(path)
		if value.Exists() {
			return value.String(), true
		}
	}
	return "", false
}

func hasNormalizedAntigravityGeminiSignature(part gjson.Result, signatureValue string) bool {
	canonicalCount := 0
	part.ForEach(
		func(key, _ gjson.Result) bool {
			if key.String() == "thoughtSignature" {
				canonicalCount++
			}
			return true
		},
	)
	if canonicalCount != 1 || part.Get("thoughtSignature").String() != signatureValue {
		return false
	}
	for _, path := range antigravityGeminiSignaturePaths[1:] {
		if part.Get(path).Exists() {
			return false
		}
	}
	return true
}

func hasAntigravityGeminiContentPart(part gjson.Result) bool {
	for _, path := range []string{
		"text", "inlineData", "inline_data", "fileData", "file_data", "functionCall", "function_call",
		"functionResponse", "function_response", "executableCode", "executable_code", "codeExecutionResult",
		"code_execution_result",
	} {
		if part.Get(path).Exists() {
			return true
		}
	}
	return false
}

func deleteAntigravityGeminiSignatureFields(part []byte) []byte {
	for _, path := range antigravityGeminiSignaturePaths {
		for gjson.GetBytes(part, path).Exists() {
			updated, errDelete := sjson.DeleteBytes(part, path)
			if errDelete != nil || len(updated) >= len(part) {
				break
			}
			part = updated
		}
	}
	return part
}

// functionCallGroup represents a group of function calls and their responses
type functionCallGroup struct {
	ResponsesNeeded int
	CallNames       []string // ordered function call names for backfilling empty response names
}

// parseFunctionResponseRaw attempts to normalize a function response part into a JSON object string.
// Falls back to a minimal "functionResponse" object when parsing fails.
// fallbackName is used when the response's own name is empty.
func parseFunctionResponseRaw(response gjson.Result, fallbackName string) string {
	if response.IsObject() && gjson.Valid(response.Raw) {
		raw := response.Raw
		name := response.Get("functionResponse.name").String()
		if strings.TrimSpace(name) == "" && fallbackName != "" {
			updated, _ := sjson.SetBytes([]byte(raw), "functionResponse.name", fallbackName)
			raw = string(updated)
		}
		return raw
	}

	log.Debugf("parse function response failed, using fallback")
	funcResp := response.Get("functionResponse")
	if funcResp.Exists() {
		fr := []byte(`{"functionResponse":{"name":"","response":{"result":""}}}`)
		name := funcResp.Get("name").String()
		if strings.TrimSpace(name) == "" {
			name = fallbackName
		}
		fr, _ = sjson.SetBytes(fr, "functionResponse.name", name)
		fr, _ = sjson.SetBytes(fr, "functionResponse.response.result", funcResp.Get("response").String())
		if id := funcResp.Get("id").String(); id != "" {
			fr, _ = sjson.SetBytes(fr, "functionResponse.id", id)
		}
		return string(fr)
	}

	useName := fallbackName
	if useName == "" {
		useName = "unknown"
	}
	fr := []byte(`{"functionResponse":{"name":"","response":{"result":""}}}`)
	fr, _ = sjson.SetBytes(fr, "functionResponse.name", useName)
	fr, _ = sjson.SetBytes(fr, "functionResponse.response.result", response.String())
	return string(fr)
}

// fixCLIToolResponse performs sophisticated tool response format conversion and grouping.
// This function transforms the CLI tool response format by intelligently grouping function calls
// with their corresponding responses, ensuring proper conversation flow and API compatibility.
// It converts from a linear format (1.json) to a grouped format (2.json) where function calls
// and their responses are properly associated and structured.
//
// Parameters:
//   - input: The input JSON to be processed
//
// Returns:
//   - []byte: The processed JSON with grouped function calls and responses
//   - error: An error if the processing fails
func fixCLIToolResponse(input []byte) ([]byte, error) {
	// The parsed result references input directly. Do not mutate input while the
	// result and its raw slices are in use.
	parsed := util.ParseGJSONBytesNoCopy(input)
	contents := parsed.Get("request.contents")
	if !contents.Exists() {
		return input, fmt.Errorf("contents not found in input")
	}

	needsGrouping := false
	allContentsAreObjects := true
	contents.ForEach(
		func(_, content gjson.Result) bool {
			if !content.IsObject() {
				allContentsAreObjects = false
				return true
			}
			content.Get("parts").ForEach(
				func(_, part gjson.Result) bool {
					if part.Get("functionResponse").Exists() {
						needsGrouping = true
						return false
					}
					return true
				},
			)
			return !needsGrouping
		},
	)
	if contents.IsArray() && allContentsAreObjects && !needsGrouping {
		return input, nil
	}

	contentItems := translatorcommon.NewRawArrayItems(contents.Get("#").Int())
	var pendingGroups []*functionCallGroup
	var collectedResponses []gjson.Result
	appendFunctionResponses := func(responses []gjson.Result, callNames []string) {
		partItems := make([][]byte, 0, len(responses))
		for responseIndex, response := range responses {
			partRaw := parseFunctionResponseRaw(response, callNames[responseIndex])
			if partRaw != "" {
				partItems = append(partItems, []byte(partRaw))
			}
		}
		if len(partItems) > 0 {
			functionResponseContent := []byte(`{"parts":[],"role":"function"}`)
			functionResponseContent, _ = sjson.SetRawBytes(
				functionResponseContent,
				"parts",
				translatorcommon.JoinRawArray(partItems),
			)
			contentItems = append(contentItems, functionResponseContent)
		}
	}

	contents.ForEach(
		func(_, value gjson.Result) bool {
			role := value.Get("role").String()
			parts := value.Get("parts")

			var responsePartsInThisContent []gjson.Result
			parts.ForEach(
				func(_, part gjson.Result) bool {
					if part.Get("functionResponse").Exists() {
						responsePartsInThisContent = append(responsePartsInThisContent, part)
					}
					return true
				},
			)

			if len(responsePartsInThisContent) > 0 {
				collectedResponses = append(collectedResponses, responsePartsInThisContent...)
				for len(pendingGroups) > 0 && len(collectedResponses) >= pendingGroups[0].ResponsesNeeded {
					group := pendingGroups[0]
					pendingGroups = pendingGroups[1:]
					groupResponses := collectedResponses[:group.ResponsesNeeded]
					collectedResponses = collectedResponses[group.ResponsesNeeded:]
					appendFunctionResponses(groupResponses, group.CallNames)
				}
				return true
			}

			if role == "model" {
				var callNames []string
				parts.ForEach(
					func(_, part gjson.Result) bool {
						if part.Get("functionCall").Exists() {
							callNames = append(callNames, part.Get("functionCall.name").String())
						}
						return true
					},
				)
				if len(callNames) > 0 {
					if !value.IsObject() {
						log.Warn("failed to parse model content")
						return true
					}
					contentItems = append(contentItems, []byte(value.Raw))
					pendingGroups = append(
						pendingGroups, &functionCallGroup{
							ResponsesNeeded: len(callNames),
							CallNames:       callNames,
						},
					)
					return true
				}
			}
			if !value.IsObject() {
				log.Warn("failed to parse content")
				return true
			}
			contentItems = append(contentItems, []byte(value.Raw))
			return true
		},
	)

	for _, group := range pendingGroups {
		if len(collectedResponses) >= group.ResponsesNeeded {
			groupResponses := collectedResponses[:group.ResponsesNeeded]
			collectedResponses = collectedResponses[group.ResponsesNeeded:]
			appendFunctionResponses(groupResponses, group.CallNames)
		}
	}

	result, _ := sjson.SetRawBytes(
		input,
		"request.contents",
		translatorcommon.JoinRawArray(contentItems),
	)
	return result, nil
}
