package responses

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/misc"
	sigcompat "github.com/Pyrokine/CLIProxyAPI/v6/internal/signature"
	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/gemini/common"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const geminiResponsesThoughtSignature = "skip_thought_signature_validator"

func ConvertOpenAIResponsesRequestToGemini(modelName string, inputRawJSON []byte, stream bool) []byte {
	converted, errConvert := convertOpenAIResponsesRequestToGemini(modelName, inputRawJSON, stream)
	if errConvert != nil {
		log.Errorf("gemini responses request translation rejected: %v", errConvert)
		return nil
	}
	return converted
}

// ConvertOpenAIResponsesRequestToGeminiChecked preserves translation errors across bridge converters.
func ConvertOpenAIResponsesRequestToGeminiChecked(modelName string, body []byte, stream bool, nativeWebSearch ...*bool) ([]byte, error) {
	return convertOpenAIResponsesRequestToGemini(modelName, body, stream, nativeWebSearch...)
}

func convertOpenAIResponsesRequestToGemini(
	modelName string,
	inputRawJSON []byte,
	stream bool,
	nativeWebSearch ...*bool,
) ([]byte, error) {
	var drops translatorcommon.UserTurnDrops
	rawJSON := inputRawJSON

	// Note: stream parameter is part of the fixed method signature
	useGeminiNativeReasoningLayout := sigcompat.ProviderFromModelName(modelName) == sigcompat.ProviderGemini
	_ = stream // Unused but required by interface

	// Base Gemini API template (do not include thinkingConfig by default)
	out := []byte(`{"contents":[]}`)

	root := gjson.ParseBytes(rawJSON)
	toolRegistry := newGeminiResponsesToolRegistry(rawJSON)
	functionDeclarations := buildGeminiResponsesFunctionDeclarations(toolRegistry)
	var toolBlocks [][]byte
	if hasResponsesWebSearchTool(root) && modelSupportsResponsesWebSearch(modelName, nativeWebSearch...) && allowsResponsesWebSearchChoice(root) {
		googleSearch := []byte(`{"googleSearch":{}}`)
		domains, errDomains := responsesWebSearchAllowedDomains(root)
		if errDomains != nil {
			return nil, errDomains
		}
		if len(domains) > 0 {
			googleSearch, _ = sjson.SetBytes(googleSearch, "googleSearch.includedDomains", domains)
		}
		toolBlocks = append(toolBlocks, googleSearch)
	}
	if len(functionDeclarations) > 0 {
		functionTool := []byte(`{"functionDeclarations":[]}`)
		functionTool, _ = sjson.SetRawBytes(
			functionTool, "functionDeclarations", translatorcommon.JoinRawArray(functionDeclarations),
		)
		toolBlocks = append(toolBlocks, functionTool)
	}
	if len(toolBlocks) > 0 {
		out, _ = sjson.SetRawBytes(out, "tools", translatorcommon.JoinRawArray(toolBlocks))
	}
	if len(functionDeclarations) > 0 {
		if toolChoice := root.Get("tool_choice"); toolChoice.Exists() {
			toolConfig, ok, errToolChoice := convertResponsesToolChoiceToGemini(toolChoice, toolRegistry)
			if errToolChoice != nil {
				return nil, errToolChoice
			}
			if ok {
				out, _ = sjson.SetRawBytes(out, "toolConfig.functionCallingConfig", toolConfig)
			}
		}
	}

	// Extract system instruction from OpenAI "instructions" field.
	systemParts := make([][]byte, 0, 2)
	if instructions := root.Get("instructions"); instructions.Exists() {
		part := []byte(`{"text":""}`)
		part, _ = sjson.SetBytes(part, "text", instructions.String())
		systemParts = append(systemParts, part)
	}

	// Convert input messages to Gemini contents format
	if input := root.Get("input"); input.Exists() && input.IsArray() {
		inputItems, hasGeminiCarrier := normalizeGeminiResponsesCarriers(input.Array())
		if hasGeminiCarrier {
			useGeminiNativeReasoningLayout = true
		}
		inputItems = translatorcommon.DeduplicateToolResultsByRawID(
			inputItems,
			func(item gjson.Result) (string, bool) {
				if !isOpenAIResponsesToolOutput(item) {
					return "", false
				}
				callID := translatorcommon.ExtractResponsesCallID(item)
				return callID, callID != ""
			},
		)
		inputItems = translatorcommon.NormalizeResponsesToolCallOutputs(inputItems)
		var errToolNames error
		inputItems, errToolNames = normalizeOpenAIResponsesToolNames(inputItems, toolRegistry)
		if errToolNames != nil {
			return nil, errToolNames
		}
		items := pairOpenAIResponsesReasoningWithFunctionCalls(inputItems)
		contentItems := make([][]byte, 0, len(items))
		functionNamesByCallID := make(map[string]string)
		pendingFunctionCallIDs := make([]string, 0)
		for _, item := range items {
			if isOpenAIResponsesToolCall(item) {
				callID := translatorcommon.ExtractResponsesCallID(item)
				if _, exists := functionNamesByCallID[callID]; !exists {
					functionNamesByCallID[callID] = openAIResponsesGeminiToolName(item)
				}
			}
		}

		normalized := items
		if useGeminiNativeReasoningLayout {
			normalized = reorderOpenAIResponsesDetachedReasoning(normalized)
		}
		consumedFunctionOutputIndexes := make(map[int]bool)
		hasEncounteredConversation := false
		var pendingDeveloperParts [][]byte
		for i := 0; i < len(normalized); i++ {
			if consumedFunctionOutputIndexes[i] {
				continue
			}
			item := normalized[i]
			itemType := item.Get("type").String()
			itemRole := item.Get("role").String()
			if itemType == "" && itemRole != "" {
				itemType = "message"
			} else if isResponsesContentPartType(itemType) && itemRole == "" {
				itemType = "message"
				itemRole = "user"
			}

			switch itemType {
			case "message":
				if strings.EqualFold(itemRole, "system") || strings.EqualFold(itemRole, "developer") {
					if !hasEncounteredConversation {
						pendingFunctionCallIDs = nil
						if contentArray := item.Get("content"); contentArray.Exists() {
							if contentArray.IsArray() {
								contentArray.ForEach(
									func(_, contentItem gjson.Result) bool {
										part := []byte(`{"text":""}`)
										part, _ = sjson.SetBytes(part, "text", contentItem.Get("text").String())
										systemParts = append(systemParts, part)
										return true
									},
								)
							} else if contentArray.Type == gjson.String {
								part := []byte(`{"text":""}`)
								part, _ = sjson.SetBytes(part, "text", contentArray.String())
								systemParts = append(systemParts, part)
							}
						}
						continue
					}

					if reminder := geminiResponsesReminderText(item.Get("content")); reminder != "" {
						part := []byte(`{"text":""}`)
						part, _ = sjson.SetBytes(part, "text", reminder)
						if len(pendingFunctionCallIDs) > 0 {
							pendingDeveloperParts = append(pendingDeveloperParts, part)
						} else {
							contentItems = append(contentItems, geminiContent("user", [][]byte{part}))
						}
					}
					continue
				}

				hasEncounteredConversation = true
				if _, isAssistantOutput := openAIResponsesAssistantVisibleText(item); !isAssistantOutput {
					if len(pendingFunctionCallIDs) > 0 {
						anyHasFutureOutput := false
						for _, callID := range pendingFunctionCallIDs {
							if responsesHasMatchingOutput(normalized[i:], callID) {
								anyHasFutureOutput = true
								break
							}
						}
						if !anyHasFutureOutput {
							synthesizedParts := make([][]byte, 0, len(pendingFunctionCallIDs))
							for _, callID := range pendingFunctionCallIDs {
								synthesizedParts = append(
									synthesizedParts,
									buildOpenAIResponsesSynthesizedFunctionResponsePart(callID, functionNamesByCallID),
								)
							}
							if len(synthesizedParts) > 0 {
								contentItems = append(contentItems, geminiContent("user", synthesizedParts))
							}
							pendingFunctionCallIDs = nil
						}
					}
					if len(pendingDeveloperParts) > 0 {
						contentItems = append(contentItems, geminiContent("user", pendingDeveloperParts))
						pendingDeveloperParts = nil
					}
				}

				// Handle regular messages and top-level content parts.
				contentArray := item.Get("content")
				var partsToProcess []gjson.Result
				if contentArray.IsArray() {
					partsToProcess = contentArray.Array()
				} else if isResponsesContentPartType(item.Get("type").String()) {
					partsToProcess = append(partsToProcess, item)
					for i+1 < len(normalized) {
						next := normalized[i+1]
						if next.Get("role").String() != "" || !isResponsesContentPartType(next.Get("type").String()) {
							break
						}
						partsToProcess = append(partsToProcess, next)
						i++
					}
				}
				if len(partsToProcess) > 0 {
					currentRole := ""
					sendable := 0
					currentParts := make([][]byte, 0)

					flush := func() {
						if currentRole == "" || len(currentParts) == 0 {
							currentParts = currentParts[:0]
							return
						}
						contentItems = append(contentItems, geminiContent(currentRole, currentParts))
						currentParts = currentParts[:0]
					}

					for _, contentItem := range partsToProcess {
						contentType := contentItem.Get("type").String()
						if contentType == "" {
							contentType = "input_text"
						}

						effRole := "user"
						if itemRole != "" {
							switch strings.ToLower(itemRole) {
							case "assistant", "model":
								effRole = "model"
							default:
								effRole = strings.ToLower(itemRole)
							}
						}
						if contentType == "output_text" {
							effRole = "model"
						}
						if effRole == "assistant" {
							effRole = "model"
						}

						if currentRole != "" && effRole != currentRole {
							flush()
							currentRole = ""
						}
						if currentRole == "" {
							currentRole = effRole
						}

						var partJSON []byte
						switch contentType {
						case "input_text", "output_text", "text":
							if text := contentItem.Get("text"); text.Exists() {
								partJSON = []byte(`{"text":""}`)
								partJSON, _ = sjson.SetBytes(partJSON, "text", text.String())
							}
						default:
							var errMedia error
							partJSON, errMedia = openAIResponsesPartFromBlock(contentItem)
							if errMedia != nil {
								var unsupported *translatorcommon.UnsupportedPartError
								if effRole == "user" && errors.As(errMedia, &unsupported) {
									drops.Drop(unsupported.Type)
									continue
								}
								return nil, errMedia
							}
						}

						if len(partJSON) > 0 {
							currentParts = append(currentParts, partJSON)
							if effRole == "user" && translatorcommon.GeminiPartIsSendable(partJSON) {
								sendable++
							}
						}
					}

					flush()
					if itemRole == "user" || itemRole == "" {
						drops.EndTurn(sendable)
					}
				} else if contentArray.Type == gjson.String {
					effRole := "user"
					if itemRole != "" {
						switch strings.ToLower(itemRole) {
						case "assistant", "model":
							effRole = "model"
						default:
							effRole = strings.ToLower(itemRole)
						}
					}

					part := []byte(`{"text":""}`)
					part, _ = sjson.SetBytes(part, "text", contentArray.String())
					contentItems = append(contentItems, geminiContent(effRole, [][]byte{part}))
				}

			case "function_call", "custom_tool_call":
				hasEncounteredConversation = true
				signature := geminiResponsesThoughtSignature
				if rawSignature := strings.TrimSpace(item.Get("_cpa_reasoning_signature").String()); rawSignature != "" {
					signature = openAIResponsesGeminiThoughtSignature(rawSignature)
				}
				if thoughtText := item.Get("_cpa_reasoning_summary").String(); thoughtText != "" {
					contentItems = append(
						contentItems,
						buildOpenAIResponsesReasoningFunctionCallModelContent(thoughtText, item, signature),
					)
				} else if !useGeminiNativeReasoningLayout && strings.TrimSpace(item.Get("_cpa_reasoning_signature").String()) != "" {
					contentItems = append(
						contentItems, buildOpenAIResponsesEmptyReasoningFunctionCallModelContent(item, signature),
					)
				} else {
					contentItems = append(contentItems, buildOpenAIResponsesFunctionCallModelContent(item, signature))
				}
				if callID := translatorcommon.ExtractResponsesCallID(item); callID != "" {
					pendingFunctionCallIDs = append(pendingFunctionCallIDs, callID)
				}

			case "function_call_output", "custom_tool_call_output":
				hasEncounteredConversation = true
				orderedOutputs, consumedIndexes, _ := collectOpenAIResponsesFunctionCallOutputs(
					normalized, i, pendingFunctionCallIDs,
				)
				for consumedIndex := range consumedIndexes {
					consumedFunctionOutputIndexes[consumedIndex] = true
				}
				end := i + len(consumedIndexes)
				outputByCallID := make(map[string]gjson.Result)
				var extraOutputs []gjson.Result
				var standaloneOutputContents [][]byte
				appendStandaloneOutput := func(output gjson.Result) {
					if parts := buildOpenAIResponsesStandaloneOutputTextParts(output); len(parts) > 0 {
						standaloneOutputContents = append(standaloneOutputContents, geminiContent("user", parts))
					}
				}
				for _, output := range orderedOutputs {
					callID := translatorcommon.ExtractResponsesCallID(output)
					if callID != "" {
						outputByCallID[callID] = output
					} else {
						extraOutputs = append(extraOutputs, output)
					}
				}

				responseParts := make([][]byte, 0, len(pendingFunctionCallIDs)+len(extraOutputs))
				stillPending := make([]string, 0, len(pendingFunctionCallIDs))
				for _, pendingID := range pendingFunctionCallIDs {
					if output, ok := outputByCallID[pendingID]; ok {
						parts, errOutput := buildOpenAIResponsesFunctionResponseParts(output, functionNamesByCallID)
						if errOutput != nil {
							return nil, errOutput
						}
						responseParts = append(responseParts, parts...)
						delete(outputByCallID, pendingID)
					} else if !responsesHasMatchingOutput(normalized[end:], pendingID) {
						responseParts = append(
							responseParts,
							buildOpenAIResponsesSynthesizedFunctionResponsePart(pendingID, functionNamesByCallID),
						)
					} else {
						stillPending = append(stillPending, pendingID)
					}
				}
				for _, output := range orderedOutputs {
					callID := translatorcommon.ExtractResponsesCallID(output)
					if _, remaining := outputByCallID[callID]; remaining {
						appendStandaloneOutput(output)
						delete(outputByCallID, callID)
					}
				}
				for _, output := range extraOutputs {
					appendStandaloneOutput(output)
				}

				pendingFunctionCallIDs = stillPending
				if len(responseParts) > 0 {
					contentItems = append(contentItems, geminiContent("user", responseParts))
				}
				contentItems = append(contentItems, standaloneOutputContents...)
				if len(pendingFunctionCallIDs) == 0 && len(pendingDeveloperParts) > 0 {
					contentItems = append(contentItems, geminiContent("user", pendingDeveloperParts))
					pendingDeveloperParts = nil
				}

			case "reasoning":
				hasEncounteredConversation = true
				thoughtText := item.Get("summary.0.text").String()
				rawSignature := item.Get("encrypted_content").String()
				carrierDirection := geminiResponsesCarrierDirection(item)
				carrierTarget := geminiResponsesCarrierTarget(item)
				if strings.TrimSpace(rawSignature) == "" && i+1 < len(normalized) {
					nextReasoning := normalized[i+1]
					if nextReasoning.Get("type").String() == "reasoning" && strings.Contains(
						nextReasoning.Get("id").String(), "_detached_after_",
					) && strings.TrimSpace(nextReasoning.Get("summary.0.text").String()) == "" && strings.TrimSpace(nextReasoning.Get("encrypted_content").String()) != "" {
						rawSignature = nextReasoning.Get("encrypted_content").String()
						i++
					}
				}
				signature := openAIResponsesGeminiThoughtSignature(rawSignature)

				visibleText := ""
				if useGeminiNativeReasoningLayout && i+1 < len(normalized) {
					next := normalized[i+1]
					canBindText := (carrierDirection == "" || carrierDirection == geminiResponsesCarrierNext) && (carrierTarget == "" || carrierTarget == geminiResponsesCarrierText || carrierTarget == geminiResponsesCarrierAny)
					canBindFunction := (carrierDirection == "" || carrierDirection == geminiResponsesCarrierNext) && (carrierTarget == "" || carrierTarget == geminiResponsesCarrierFunction || carrierTarget == geminiResponsesCarrierAny)
					if visible, ok := openAIResponsesAssistantVisibleText(next); ok && canBindText {
						visibleText = visible
						i++
					} else if isOpenAIResponsesToolCall(next) && canBindFunction && strings.TrimSpace(next.Get("_cpa_reasoning_signature").String()) == "" && signature != geminiResponsesThoughtSignature {
						contentItems = append(
							contentItems,
							buildOpenAIResponsesReasoningFunctionCallModelContent(thoughtText, next, signature),
						)
						if callID := translatorcommon.ExtractResponsesCallID(next); callID != "" {
							pendingFunctionCallIDs = append(pendingFunctionCallIDs, callID)
						}
						i++
						continue
					}
				}

				if modelContent := buildOpenAIResponsesReasoningModelContent(
					thoughtText, visibleText, signature, useGeminiNativeReasoningLayout,
				); len(modelContent) > 0 {
					contentItems = append(contentItems, modelContent)
				}
			}
		}
		if len(pendingDeveloperParts) > 0 {
			contentItems = append(contentItems, geminiContent("user", pendingDeveloperParts))
		}
		contentItems = coalesceAdjacentOpenAIResponsesModelContents(contentItems)
		contentItems = mergeOpenAIResponsesUserContents(contentItems)
		out = translatorcommon.SetRawArrayItems(out, "contents", contentItems)
	} else if input.Exists() && input.Type == gjson.String {
		// Simple string input conversion to user message.
		part := []byte(`{"text":""}`)
		part, _ = sjson.SetBytes(part, "text", input.String())
		out = translatorcommon.SetRawArrayItems(out, "contents", [][]byte{geminiContent("user", [][]byte{part})})
	}
	if len(systemParts) > 0 {
		out, _ = sjson.SetRawBytes(out, "systemInstruction", geminiSystemInstruction(systemParts))
	}

	// Handle generation config from OpenAI format
	if maxOutputTokens := root.Get("max_output_tokens"); maxOutputTokens.Exists() {
		genConfig := []byte(`{"maxOutputTokens":0}`)
		genConfig, _ = sjson.SetBytes(genConfig, "maxOutputTokens", maxOutputTokens.Int())
		out, _ = sjson.SetRawBytes(out, "generationConfig", genConfig)
	}

	// Handle temperature if present
	if temperature := root.Get("temperature"); temperature.Exists() {
		out, _ = sjson.SetBytes(out, "generationConfig.temperature", temperature.Float())
	}

	// Handle top_p if present
	if topP := root.Get("top_p"); topP.Exists() {
		out, _ = sjson.SetBytes(out, "generationConfig.topP", topP.Float())
	}

	// Handle stop sequences
	if stopSequences := root.Get("stop_sequences"); stopSequences.Exists() && stopSequences.IsArray() {
		var sequences []string
		stopSequences.ForEach(
			func(_, seq gjson.Result) bool {
				sequences = append(sequences, seq.String())
				return true
			},
		)
		out, _ = sjson.SetBytes(out, "generationConfig.stopSequences", sequences)
	}

	out = applyOpenAIResponsesTextFormatToGemini(out, root)

	// Apply thinking configuration: convert OpenAI Responses API reasoning.effort to Gemini thinkingConfig.
	// Inline translation-only mapping; capability checks happen later in ApplyThinking.
	re := root.Get("reasoning.effort")
	if re.Exists() {
		effort := strings.ToLower(strings.TrimSpace(re.String()))
		if effort != "" {
			thinkingPath := "generationConfig.thinkingConfig"
			if effort == "auto" {
				out, _ = sjson.SetBytes(out, thinkingPath+".thinkingBudget", -1)
			} else {
				out, _ = sjson.SetBytes(out, thinkingPath+".thinkingLevel", effort)
			}
		}
	}

	result := common.AttachDefaultSafetySettings(out, "safetySettings")
	if useGeminiNativeReasoningLayout {
		result = sigcompat.SanitizeGeminiRequestThoughtSignatures(result, "contents")
	}
	if err := drops.Err(); err != nil {
		return nil, err
	}
	return stripTrailingOpenAIResponsesModelPrefill(result), nil
}

type geminiResponsesToolRegistry struct {
	identity *translatorcommon.ResponsesToolIdentity
	forward  map[string]string
	reverse  map[string]string
}

func newGeminiResponsesToolRegistry(rawJSON []byte) *geminiResponsesToolRegistry {
	requestJSON := rawJSON
	if root := gjson.ParseBytes(rawJSON); root.Get("request").IsObject() {
		requestJSON = []byte(root.Get("request").Raw)
	}
	identity := translatorcommon.NewResponsesToolIdentity(requestJSON)
	declarations := identity.Declarations()
	syntheticRequest := []byte(`{"tools":[]}`)
	for _, declaration := range declarations {
		tool := []byte(`{"name":""}`)
		tool, _ = sjson.SetBytes(tool, "name", declaration.QualifiedName)
		syntheticRequest, _ = sjson.SetRawBytes(syntheticRequest, "tools.-1", tool)
	}
	forward := util.SanitizedFunctionNameMap(syntheticRequest)
	reverse := make(map[string]string, len(forward))
	for qualifiedName, geminiName := range forward {
		reverse[geminiName] = qualifiedName
	}
	return &geminiResponsesToolRegistry{identity: identity, forward: forward, reverse: reverse}
}

func (registry *geminiResponsesToolRegistry) geminiName(qualifiedName string) string {
	if registry != nil {
		if mapped := registry.forward[qualifiedName]; mapped != "" {
			return mapped
		}
	}
	return util.SanitizeFunctionName(qualifiedName)
}

func (registry *geminiResponsesToolRegistry) qualifiedName(geminiName string) string {
	if registry != nil {
		if qualifiedName := registry.reverse[geminiName]; qualifiedName != "" {
			return qualifiedName
		}
	}
	return geminiName
}

func (registry *geminiResponsesToolRegistry) resolveInputName(name, namespace string) (string, error) {
	name = strings.TrimSpace(name)
	namespace = strings.TrimSpace(strings.TrimSuffix(namespace, "__"))
	if name == "" {
		return "", nil
	}
	if registry != nil && registry.identity != nil {
		declaration, errResolve := registry.identity.ResolveDeclaration(name, namespace)
		if errResolve == nil {
			return registry.geminiName(declaration.QualifiedName), nil
		}
		if errors.Is(errResolve, translatorcommon.ErrResponsesToolNameAmbiguous) {
			return "", errResolve
		}
	}
	qualifiedName := name
	if namespace != "" && name != namespace && !strings.HasPrefix(name, namespace+"__") {
		qualifiedName = namespace + "__" + name
	}
	return registry.geminiName(qualifiedName), nil
}

func buildGeminiResponsesFunctionDeclarations(registry *geminiResponsesToolRegistry) [][]byte {
	if registry == nil || registry.identity == nil {
		return nil
	}
	declarations := registry.identity.Declarations()
	functionDeclarations := make([][]byte, 0, len(declarations))
	for _, declaration := range declarations {
		tool := gjson.Parse(declaration.Raw)
		functionDeclaration := []byte(`{"name":"","description":"","parametersJsonSchema":{}}`)
		functionDeclaration, _ = sjson.SetBytes(
			functionDeclaration, "name", registry.geminiName(declaration.QualifiedName),
		)
		if description := responsesToolDescription(tool); description != "" {
			functionDeclaration, _ = sjson.SetBytes(functionDeclaration, "description", description)
		}
		if declaration.Kind == translatorcommon.ResponsesCustomTool {
			functionDeclaration, _ = sjson.SetRawBytes(
				functionDeclaration,
				"parametersJsonSchema",
				[]byte(`{"type":"object","properties":{"input":{"type":"string"}},"required":["input"]}`),
			)
		} else if parameters := responsesToolParameters(tool); parameters.Exists() {
			functionDeclaration, _ = sjson.SetRawBytes(
				functionDeclaration,
				"parametersJsonSchema",
				[]byte(util.CleanJSONSchemaForGeminiJSONSchema(parameters.Raw)),
			)
		}
		functionDeclarations = append(functionDeclarations, functionDeclaration)
	}
	return functionDeclarations
}

func responsesToolDescription(tool gjson.Result) string {
	if description := tool.Get("description").String(); description != "" {
		return description
	}
	return tool.Get("function.description").String()
}

func responsesToolParameters(tool gjson.Result) gjson.Result {
	for _, path := range []string{
		"parameters",
		"parametersJsonSchema",
		"input_schema",
		"function.parameters",
		"function.parametersJsonSchema",
	} {
		if parameters := tool.Get(path); parameters.Exists() {
			return parameters
		}
	}
	return gjson.Result{}
}

func normalizeOpenAIResponsesToolNames(
	items []gjson.Result,
	registry *geminiResponsesToolRegistry,
) ([]gjson.Result, error) {
	normalized := append([]gjson.Result(nil), items...)
	toolNamesByCallID := make(map[string]string)
	for index, item := range normalized {
		if !isOpenAIResponsesToolCall(item) {
			continue
		}
		geminiName, errResolve := registry.resolveInputName(
			item.Get("name").String(), item.Get("namespace").String(),
		)
		if errResolve != nil {
			return nil, fmt.Errorf("tool call %q: %w", item.Get("name").String(), errResolve)
		}
		if geminiName == "" {
			continue
		}
		raw, _ := sjson.SetBytes([]byte(item.Raw), "_cpa_gemini_tool_name", geminiName)
		normalized[index] = gjson.ParseBytes(raw)
		if callID := translatorcommon.ExtractResponsesCallID(item); callID != "" {
			toolNamesByCallID[callID] = geminiName
		}
	}
	for index, item := range normalized {
		if !isOpenAIResponsesToolOutput(item) {
			continue
		}
		callID := translatorcommon.ExtractResponsesCallID(item)
		geminiName := toolNamesByCallID[callID]
		if geminiName == "" && strings.TrimSpace(item.Get("name").String()) != "" {
			var errResolve error
			geminiName, errResolve = registry.resolveInputName(
				item.Get("name").String(), item.Get("namespace").String(),
			)
			if errResolve != nil {
				return nil, fmt.Errorf("tool output %q: %w", item.Get("name").String(), errResolve)
			}
		}
		if geminiName != "" {
			raw, _ := sjson.SetBytes([]byte(item.Raw), "_cpa_gemini_tool_name", geminiName)
			normalized[index] = gjson.ParseBytes(raw)
		}
	}
	return normalized, nil
}

func openAIResponsesGeminiToolName(item gjson.Result) string {
	if name := strings.TrimSpace(item.Get("_cpa_gemini_tool_name").String()); name != "" {
		return name
	}
	return util.SanitizeFunctionName(item.Get("name").String())
}

func convertResponsesToolChoiceToGemini(
	toolChoice gjson.Result,
	registry *geminiResponsesToolRegistry,
) ([]byte, bool, error) {
	mode := ""
	var allowedNames []string
	if toolChoice.Type == gjson.String {
		switch strings.ToLower(strings.TrimSpace(toolChoice.String())) {
		case "none":
			mode = "NONE"
		case "auto":
			mode = "AUTO"
		case "required", "any":
			mode = "ANY"
		}
	} else if toolChoice.IsObject() {
		toolType := strings.ToLower(strings.TrimSpace(toolChoice.Get("type").String()))
		switch toolType {
		case "none":
			mode = "NONE"
		case "auto":
			mode = "AUTO"
		case "required", "any":
			mode = "ANY"
		case "", "function", "custom", "tool":
			mode = "ANY"
			name := strings.TrimSpace(toolChoice.Get("name").String())
			if name == "" {
				name = strings.TrimSpace(toolChoice.Get("function.name").String())
			}
			if name == "" {
				name = strings.TrimSpace(toolChoice.Get("custom.name").String())
			}
			namespace := strings.TrimSpace(toolChoice.Get("namespace").String())
			if namespace == "" {
				namespace = strings.TrimSpace(toolChoice.Get("function.namespace").String())
			}
			if namespace == "" {
				namespace = strings.TrimSpace(toolChoice.Get("custom.namespace").String())
			}
			if name != "" {
				if registry == nil || registry.identity == nil {
					return nil, false, translatorcommon.ErrResponsesToolNameUnknown
				}
				declaration, errResolve := registry.identity.ResolveDeclaration(name, namespace)
				if errResolve != nil {
					return nil, false, fmt.Errorf("tool_choice %q: %w", name, errResolve)
				}
				allowedNames = append(allowedNames, registry.geminiName(declaration.QualifiedName))
			}
		}
	}
	if mode == "" {
		return nil, false, nil
	}
	config := []byte(`{"mode":""}`)
	config, _ = sjson.SetBytes(config, "mode", mode)
	if len(allowedNames) > 0 {
		config, _ = sjson.SetBytes(config, "allowedFunctionNames", allowedNames)
	}
	return config, true, nil
}

func geminiResponsesReminderText(content gjson.Result) string {
	var texts []string
	switch {
	case content.Type == gjson.String:
		texts = append(texts, content.String())
	case content.IsArray():
		for _, part := range content.Array() {
			if part.Type == gjson.String {
				texts = append(texts, part.String())
			} else if text := part.Get("text").String(); text != "" {
				texts = append(texts, text)
			}
		}
	case content.IsObject():
		texts = append(texts, content.Get("text").String())
	}
	text := strings.Join(texts, "\n")
	if strings.TrimSpace(text) == "" {
		return ""
	}
	return "<system-reminder>\n" + text + "\n</system-reminder>"
}

func geminiContent(role string, parts [][]byte) []byte {
	content := []byte(`{"role":"","parts":[]}`)
	content, _ = sjson.SetBytes(content, "role", role)
	content, _ = sjson.SetRawBytes(content, "parts", translatorcommon.JoinRawArray(parts))
	return content
}

func coalesceAdjacentOpenAIResponsesModelContents(contents [][]byte) [][]byte {
	coalesced := make([][]byte, 0, len(contents))
	for _, content := range contents {
		contentResult := gjson.ParseBytes(content)
		if !strings.EqualFold(strings.TrimSpace(contentResult.Get("role").String()), "model") || len(coalesced) == 0 {
			coalesced = append(coalesced, content)
			continue
		}
		lastIndex := len(coalesced) - 1
		lastResult := gjson.ParseBytes(coalesced[lastIndex])
		if !strings.EqualFold(strings.TrimSpace(lastResult.Get("role").String()), "model") {
			coalesced = append(coalesced, content)
			continue
		}
		merged := coalesced[lastIndex]
		parts := contentResult.Get("parts")
		if !parts.IsArray() {
			coalesced = append(coalesced, content)
			continue
		}
		parts.ForEach(
			func(_, part gjson.Result) bool {
				merged, _ = sjson.SetRawBytes(merged, "parts.-1", []byte(part.Raw))
				return true
			},
		)
		coalesced[lastIndex] = merged
	}
	return coalesced
}

func mergeOpenAIResponsesUserContents(contents [][]byte) [][]byte {
	merged := make([][]byte, 0, len(contents))
	for start := 0; start < len(contents); {
		if gjson.GetBytes(contents[start], "role").String() != "user" {
			merged = append(merged, contents[start])
			start++
			continue
		}

		end := start + 1
		for end < len(contents) && gjson.GetBytes(contents[end], "role").String() == "user" {
			end++
		}
		run := contents[start:end]
		functionResponseTurns := 0
		for _, content := range run {
			if translatorcommon.ContentHasGeminiFunctionResponse(content) {
				functionResponseTurns++
			}
		}
		if functionResponseTurns > 1 {
			merged = append(merged, translatorcommon.MergeAdjacentGeminiUserContents(run)...)
		} else {
			merged = append(merged, translatorcommon.MergeAdjacentGeminiContents(run)...)
		}
		start = end
	}
	return merged
}

func geminiSystemInstruction(parts [][]byte) []byte {
	systemInstruction := []byte(`{"parts":[]}`)
	systemInstruction, _ = sjson.SetRawBytes(systemInstruction, "parts", translatorcommon.JoinRawArray(parts))
	return systemInstruction
}

func stripTrailingOpenAIResponsesModelPrefill(payload []byte) []byte {
	contents := gjson.GetBytes(payload, "contents")
	if !contents.IsArray() {
		return payload
	}
	contentArray := contents.Array()
	if len(contentArray) == 0 || !shouldStripTrailingOpenAIResponsesModelPrefill(contentArray[len(contentArray)-1]) {
		return payload
	}
	items := make([][]byte, 0, len(contentArray)-1)
	for _, content := range contentArray[:len(contentArray)-1] {
		items = append(items, []byte(content.Raw))
	}
	if len(items) == 0 {
		updated, errSet := sjson.SetRawBytes(payload, "contents", []byte("[]"))
		if errSet == nil {
			return updated
		}
		return payload
	}
	updated, errSet := sjson.SetRawBytes(payload, "contents", translatorcommon.JoinRawArray(items))
	if errSet != nil {
		return payload
	}
	return updated
}

func shouldStripTrailingOpenAIResponsesModelPrefill(lastContent gjson.Result) bool {
	if lastContent.Get("role").String() != "model" {
		return false
	}
	parts := lastContent.Get("parts")
	if !parts.IsArray() {
		return false
	}
	for _, part := range parts.Array() {
		if part.Get("thought").Bool() || part.Get("functionCall").Exists() || strings.TrimSpace(part.Get("thoughtSignature").String()) != "" {
			return false
		}
	}
	return true
}

func openAIResponsesAssistantVisibleText(item gjson.Result) (string, bool) {
	itemType := item.Get("type").String()
	itemRole := item.Get("role").String()
	if itemType == "" && itemRole != "" {
		itemType = "message"
	}
	if itemType != "message" {
		return "", false
	}

	content := item.Get("content")
	if !content.Exists() {
		return "", false
	}
	if content.Type == gjson.String {
		switch strings.ToLower(strings.TrimSpace(itemRole)) {
		case "assistant", "model":
			return content.String(), true
		default:
			return "", false
		}
	}
	if !content.IsArray() {
		return "", false
	}

	var textParts []string
	hasOutputText := false
	content.ForEach(
		func(_, contentItem gjson.Result) bool {
			contentType := contentItem.Get("type").String()
			if contentType == "" {
				contentType = "input_text"
			}
			if contentType != "output_text" {
				return true
			}
			hasOutputText = true
			textParts = append(textParts, contentItem.Get("text").String())
			return true
		},
	)
	if !hasOutputText {
		return "", false
	}
	// output_text marks model-visible content even when message.role is "user".
	return strings.Join(textParts, "\n"), true
}

func isOpenAIResponsesToolCall(item gjson.Result) bool {
	itemType := item.Get("type").String()
	return itemType == "function_call" || itemType == "custom_tool_call"
}

func isOpenAIResponsesToolOutput(item gjson.Result) bool {
	itemType := item.Get("type").String()
	return itemType == "function_call_output" || itemType == "custom_tool_call_output"
}

func pairOpenAIResponsesReasoningWithFunctionCalls(items []gjson.Result) []gjson.Result {
	isDetachedCarrier := isOpenAIResponsesDetachedCarrier
	postCallSignature := make(map[int]string)
	postCallCarrier := make(map[int]bool)
	consumedPostCallCarrier := make(map[int]bool)
	for groupStart := 0; groupStart < len(items); {
		if !isOpenAIResponsesToolCall(items[groupStart]) && !isDetachedCarrier(items[groupStart]) {
			groupStart++
			continue
		}
		groupEnd := groupStart
		hasFunctionCall := false
		for groupEnd < len(items) && (isOpenAIResponsesToolCall(items[groupEnd]) || isDetachedCarrier(items[groupEnd])) {
			hasFunctionCall = hasFunctionCall || isOpenAIResponsesToolCall(items[groupEnd])
			groupEnd++
		}
		if !hasFunctionCall || groupEnd >= len(items) || !isOpenAIResponsesToolOutput(items[groupEnd]) {
			groupStart = groupEnd
			continue
		}
		outputEnd := groupEnd
		for outputEnd < len(items) && isOpenAIResponsesToolOutput(items[outputEnd]) {
			outputEnd++
		}
		// A run beginning with a carrier uses leading-carrier semantics. A run
		// beginning with a call uses post-call semantics. This preserves both
		// carrier,call,carrier,call and call,carrier,call,carrier histories.
		if isOpenAIResponsesToolCall(items[groupStart]) {
			for callIndex := groupStart; callIndex < groupEnd; callIndex++ {
				item := items[callIndex]
				if !isOpenAIResponsesToolCall(item) || strings.TrimSpace(item.Get("_cpa_reasoning_signature").String()) != "" || callIndex+1 >= groupEnd || !isDetachedCarrier(items[callIndex+1]) {
					continue
				}
				carrierDirection := geminiResponsesCarrierDirection(items[callIndex+1])
				carrierTarget := geminiResponsesCarrierTarget(items[callIndex+1])
				if carrierDirection != "" && (carrierDirection != geminiResponsesCarrierPrevious || (carrierTarget != geminiResponsesCarrierFunction && carrierTarget != geminiResponsesCarrierAny)) {
					continue
				}
				carrierEnd := callIndex + 1
				for carrierEnd < groupEnd && isDetachedCarrier(items[carrierEnd]) {
					postCallCarrier[carrierEnd] = true
					carrierEnd++
				}
				callID := translatorcommon.ExtractResponsesCallID(item)
				if callID == "" {
					continue
				}
				for outputIndex := groupEnd; outputIndex < outputEnd; outputIndex++ {
					if translatorcommon.ExtractResponsesCallID(items[outputIndex]) == callID {
						postCallSignature[callIndex] = strings.TrimSpace(items[callIndex+1].Get("encrypted_content").String())
						consumedPostCallCarrier[callIndex+1] = true
						break
					}
				}
			}
		}
		groupStart = outputEnd
	}

	paired := make([]gjson.Result, 0, len(items))
	for index := 0; index < len(items); index++ {
		item := items[index]
		if signature := postCallSignature[index]; signature != "" {
			functionCall := []byte(item.Raw)
			functionCall, _ = sjson.SetBytes(functionCall, "_cpa_reasoning_signature", signature)
			paired = append(paired, gjson.ParseBytes(functionCall))
			continue
		}
		if consumedPostCallCarrier[index] {
			continue
		}
		carrierDirection := geminiResponsesCarrierDirection(item)
		carrierTarget := geminiResponsesCarrierTarget(item)
		canBindFollowingCall := carrierDirection == "" || (carrierDirection == geminiResponsesCarrierNext && (carrierTarget == geminiResponsesCarrierFunction || carrierTarget == geminiResponsesCarrierAny))
		if item.Get("type").String() == "reasoning" && !postCallCarrier[index] && canBindFollowingCall && !strings.Contains(
			item.Get("id").String(), "_detached_after_",
		) && index+1 < len(items) && isOpenAIResponsesToolCall(items[index+1]) {
			rawSignature := strings.TrimSpace(item.Get("encrypted_content").String())
			if rawSignature != "" {
				functionCall := []byte(items[index+1].Raw)
				functionCall, _ = sjson.SetBytes(functionCall, "_cpa_reasoning_signature", rawSignature)
				if summary := item.Get("summary.0.text").String(); summary != "" {
					functionCall, _ = sjson.SetBytes(functionCall, "_cpa_reasoning_summary", summary)
				}
				paired = append(paired, gjson.ParseBytes(functionCall))
				index++
				continue
			}
		}
		paired = append(paired, item)
	}
	return paired
}

func reorderOpenAIResponsesDetachedReasoning(items []gjson.Result) []gjson.Result {
	reordered := make([]gjson.Result, 0, len(items))
	for itemIndex, item := range items {
		isReasoningCarrier := isOpenAIResponsesDetachedCarrier(item)
		markedDetached := strings.Contains(item.Get("id").String(), "_detached_after_")
		if isReasoningCarrier && len(reordered) > 0 {
			previous := reordered[len(reordered)-1]
			previousType := previous.Get("type").String()
			if previousType == "" && previous.Get("role").String() != "" {
				previousType = "message"
			}
			isAssistantMessage := false
			if previousType == "message" {
				_, isAssistantMessage = openAIResponsesAssistantVisibleText(previous)
			}

			direction := geminiResponsesCarrierDirection(item)
			targetKind := geminiResponsesCarrierTarget(item)
			if direction != "" {
				alreadyPairedText := false
				alreadyPairedFunction := false
				if len(reordered) > 1 {
					prior := reordered[len(reordered)-2]
					priorDirection := geminiResponsesCarrierDirection(prior)
					priorTarget := geminiResponsesCarrierTarget(prior)
					priorBindsFollowing := isOpenAIResponsesDetachedCarrier(prior) && (priorDirection == geminiResponsesCarrierNext || priorDirection == geminiResponsesCarrierPrevious)
					alreadyPairedText = priorBindsFollowing && (priorTarget == geminiResponsesCarrierText || priorTarget == geminiResponsesCarrierAny)
					alreadyPairedFunction = priorBindsFollowing && (priorTarget == geminiResponsesCarrierFunction || priorTarget == geminiResponsesCarrierAny)
				}
				bindPreviousMessage := direction == geminiResponsesCarrierPrevious && (targetKind == geminiResponsesCarrierText || targetKind == geminiResponsesCarrierAny) && isAssistantMessage && !alreadyPairedText
				bindPreviousFunction := direction == geminiResponsesCarrierPrevious && (targetKind == geminiResponsesCarrierFunction || targetKind == geminiResponsesCarrierAny) && isOpenAIResponsesToolCall(previous) && strings.TrimSpace(previous.Get("_cpa_reasoning_signature").String()) == "" && !alreadyPairedFunction
				if bindPreviousMessage || bindPreviousFunction {
					movedItemJSON, _ := sjson.SetBytes(
						[]byte(item.Raw), geminiResponsesCarrierDirectionField, geminiResponsesCarrierNext,
					)
					reordered[len(reordered)-1] = gjson.ParseBytes(movedItemJSON)
					reordered = append(reordered, previous)
					continue
				}
				reordered = append(reordered, item)
				continue
			}

			if isAssistantMessage && !markedDetached && itemIndex+1 < len(items) {
				_, nextIsAssistantMessage := openAIResponsesAssistantVisibleText(items[itemIndex+1])
				isAssistantMessage = !nextIsAssistantMessage
			}
			alreadyPaired := false
			if len(reordered) > 1 {
				prior := reordered[len(reordered)-2]
				alreadyPaired = isOpenAIResponsesDetachedCarrier(prior) && strings.Contains(
					prior.Get("id").String(), "_detached_after_",
				)
			}
			if !alreadyPaired && (isAssistantMessage || (markedDetached && isOpenAIResponsesToolCall(previous) && strings.TrimSpace(previous.Get("_cpa_reasoning_signature").String()) == "")) {
				reordered[len(reordered)-1] = item
				reordered = append(reordered, previous)
				continue
			}
		}
		reordered = append(reordered, item)
	}
	return reordered
}

func buildOpenAIResponsesFunctionCallPart(item gjson.Result, signature string) []byte {
	functionCall := []byte(`{"functionCall":{"name":"","args":{}}}`)
	functionCall, _ = sjson.SetBytes(functionCall, "functionCall.name", openAIResponsesGeminiToolName(item))
	functionCall, _ = sjson.SetBytes(functionCall, "thoughtSignature", signature)
	functionCall, _ = sjson.SetBytes(functionCall, "functionCall.id", translatorcommon.ExtractResponsesCallID(item))
	if item.Get("type").String() == "custom_tool_call" {
		arguments := translatorcommon.WrapResponsesCustomToolInput(item.Get("input"))
		functionCall, _ = sjson.SetRawBytes(functionCall, "functionCall.args", []byte(arguments))
		return functionCall
	}

	arguments := item.Get("arguments").String()
	if arguments == "" {
		return functionCall
	}
	argsResult := gjson.Parse(arguments)
	if argsResult.IsObject() || argsResult.IsArray() {
		functionCall, _ = sjson.SetRawBytes(functionCall, "functionCall.args", []byte(argsResult.Raw))
	} else {
		functionCall, _ = sjson.SetBytes(functionCall, "functionCall.args.arguments", arguments)
	}
	return functionCall
}

func geminiResponsesInlineDataPart(mimeType, data string) []byte {
	part := []byte(`{"inline_data":{"mime_type":"","data":""}}`)
	part, _ = sjson.SetBytes(part, "inline_data.mime_type", mimeType)
	part, _ = sjson.SetBytes(part, "inline_data.data", data)
	return part
}

func parseOpenAIResponsesDataURL(dataURL string) (string, string) {
	dataURL = strings.TrimSpace(dataURL)
	if len(dataURL) < 5 || !strings.EqualFold(dataURL[:5], "data:") {
		return "", ""
	}
	metadata, payload, found := strings.Cut(dataURL[5:], ",")
	if !found || strings.TrimSpace(payload) == "" {
		return "", ""
	}
	mimeType := ""
	fields := strings.Split(metadata, ";")
	if len(fields) > 0 {
		mimeType = strings.TrimSpace(fields[0])
	}
	isBase64 := false
	for _, field := range fields[1:] {
		if strings.EqualFold(strings.TrimSpace(field), "base64") {
			isBase64 = true
			break
		}
	}
	if !isBase64 {
		return "", ""
	}
	payload = strings.TrimSpace(payload)
	if _, err := base64.StdEncoding.DecodeString(payload); err != nil {
		if _, err = base64.RawStdEncoding.DecodeString(payload); err != nil {
			return "", ""
		}
	}
	return mimeType, payload
}

func isResponsesContentPartType(itemType string) bool {
	switch strings.ToLower(strings.TrimSpace(itemType)) {
	case "input_text", "output_text", "text", "input_image", "image_url", "image",
		"input_audio", "audio", "input_video", "video_url", "video", "input_file", "file":
		return true
	default:
		return false
	}
}

func firstNonEmptyMediaValue(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func mediaMIMEType(kind, format, filename string) string {
	format = strings.ToLower(strings.TrimSpace(format))
	if format == "application/octet-stream" || format == "binary/octet-stream" {
		format = ""
	}
	if strings.Contains(format, "/") {
		return format
	}
	if format == "" {
		format = strings.ToLower(strings.TrimPrefix(filepath.Ext(filename), "."))
	}
	switch format {
	case "jpg", "jpeg":
		return "image/jpeg"
	case "wav":
		return "audio/wav"
	case "mp3", "mpeg":
		if kind == "audio" {
			return "audio/mpeg"
		}
	case "mov", "quicktime":
		return "video/quicktime"
	case "avi", "x-msvideo":
		return "video/x-msvideo"
	case "pcm", "pcm16":
		return "audio/pcm"
	case "opus":
		return "audio/opus"
	case "g711_ulaw", "g711_alaw":
		return "audio/basic"
	}
	if mapped := misc.MimeTypes[format]; mapped != "" && mapped != "application/octet-stream" && mapped != "binary/octet-stream" {
		return mapped
	}
	switch kind {
	case "image":
		return "image/png"
	case "audio":
		return "audio/wav"
	case "video":
		return "video/mp4"
	default:
		return "application/octet-stream"
	}
}

func openAIResponsesPartFromBlock(block gjson.Result) ([]byte, error) {
	blockType := strings.ToLower(strings.TrimSpace(block.Get("type").String()))
	kind := ""
	switch blockType {
	case "input_image", "image_url", "image":
		kind = "image"
	case "input_audio", "audio":
		kind = "audio"
	case "input_video", "video_url", "video":
		kind = "video"
	case "input_file", "file":
		kind = "file"
	default:
		return nil, nil
	}

	filename := firstNonEmptyMediaValue(block.Get("filename").String(), block.Get("file.filename").String())
	format := ""
	for _, candidate := range []string{
		block.Get(kind + ".format").String(), block.Get("input_" + kind + ".format").String(),
		block.Get(kind + ".mime_type").String(), block.Get("input_" + kind + ".mime_type").String(),
		block.Get("format").String(), block.Get("mime_type").String(),
	} {
		candidate = strings.TrimSpace(candidate)
		if candidate != "" && candidate != "application/octet-stream" && candidate != "binary/octet-stream" {
			format = candidate
			break
		}
	}
	mediaURL := firstNonEmptyMediaValue(
		block.Get(kind+"_url.url").String(), block.Get(kind+"_url").String(),
		block.Get("file.file_url").String(), block.Get("url").String(),
	)
	if mediaURL != "" && (strings.HasPrefix(strings.ToLower(mediaURL), "https://") ||
		strings.HasPrefix(strings.ToLower(mediaURL), "http://") || strings.HasPrefix(strings.ToLower(mediaURL), "gs://")) {
		parsedURL, err := url.Parse(mediaURL)
		if err != nil || parsedURL.Host == "" {
			return nil, fmt.Errorf("invalid %s media URI", blockType)
		}
		if filename == "" {
			filename = filepath.Base(parsedURL.Path)
		}
		part := []byte(`{"file_data":{"mime_type":"","file_uri":""}}`)
		part, _ = sjson.SetBytes(part, "file_data.mime_type", mediaMIMEType(kind, format, filename))
		part, _ = sjson.SetBytes(part, "file_data.file_uri", mediaURL)
		return part, nil
	}
	data := mediaURL
	if data == "" {
		data = firstNonEmptyMediaValue(
			block.Get("input_"+kind+".data").String(), block.Get(kind+".data").String(),
			block.Get("file.file_data").String(), block.Get("file_data").String(), block.Get("data").String(),
		)
	}
	if data == "" && block.Get("source.type").String() == "base64" {
		data = firstNonEmptyMediaValue(block.Get("source.data").String())
		if format == "" {
			format = block.Get("source.media_type").String()
		}
	}
	if data == "" {
		if block.Get("file_id").String() != "" || block.Get("file.file_id").String() != "" {
			return nil, &translatorcommon.UnsupportedPartError{Type: blockType}
		}
		return nil, fmt.Errorf("missing %s media data", blockType)
	}
	mimeType := mediaMIMEType(kind, format, filename)
	if strings.HasPrefix(strings.ToLower(data), "data:") {
		parsedType, payload := parseOpenAIResponsesDataURL(data)
		if payload == "" {
			return nil, fmt.Errorf("invalid %s data URL", blockType)
		}
		if parsedType != "" && parsedType != "application/octet-stream" && parsedType != "binary/octet-stream" {
			mimeType = parsedType
		}
		data = payload
	} else if strings.Contains(data, "://") {
		return nil, fmt.Errorf("unsupported %s media URI", blockType)
	}
	return geminiResponsesInlineDataPart(mimeType, data), nil
}

type openAIResponsesOutputBlock struct {
	text   string
	isText bool
	raw    string
}

func parseOpenAIResponsesArrayOutput(output gjson.Result) (string, bool, [][]byte, error) {
	var imageParts [][]byte
	var nonImageEntries []openAIResponsesOutputBlock
	hasContentBlock := false
	hasNonTextBlock := false
	var errMedia error
	output.ForEach(
		func(_, block gjson.Result) bool {
			part, err := openAIResponsesPartFromBlock(block)
			if err != nil {
				errMedia = err
				return false
			}
			if len(part) > 0 {
				hasContentBlock = true
				imageParts = append(imageParts, part)
				return true
			}
			blockType := block.Get("type").String()
			if blockType == "input_text" || blockType == "output_text" || blockType == "text" {
				hasContentBlock = true
				nonImageEntries = append(
					nonImageEntries,
					openAIResponsesOutputBlock{text: block.Get("text").String(), isText: true, raw: block.Raw},
				)
			} else if block.Type == gjson.String {
				nonImageEntries = append(
					nonImageEntries,
					openAIResponsesOutputBlock{text: block.String(), isText: true, raw: block.Raw},
				)
			} else {
				hasNonTextBlock = true
				nonImageEntries = append(
					nonImageEntries,
					openAIResponsesOutputBlock{text: block.Raw, raw: block.Raw},
				)
			}
			return true
		},
	)
	if errMedia != nil {
		return "", false, nil, errMedia
	}
	if !hasContentBlock {
		return output.Raw, true, nil, nil
	}
	switch len(nonImageEntries) {
	case 0:
		return "", false, imageParts, nil
	case 1:
		if nonImageEntries[0].isText {
			return nonImageEntries[0].text, false, imageParts, nil
		}
		return nonImageEntries[0].raw, true, imageParts, nil
	default:
		if !hasNonTextBlock {
			texts := make([]string, len(nonImageEntries))
			for index, entry := range nonImageEntries {
				texts[index] = entry.text
			}
			return strings.Join(texts, "\n"), false, imageParts, nil
		}
		rawItems := make([][]byte, len(nonImageEntries))
		for index, entry := range nonImageEntries {
			rawItems[index] = []byte(entry.raw)
		}
		return string(translatorcommon.JoinRawArray(rawItems)), true, imageParts, nil
	}
}

func buildOpenAIResponsesStandaloneOutputTextParts(item gjson.Result) [][]byte {
	output := item.Get("output")
	if !output.Exists() {
		log.Warn("Gemini Responses orphan tool output has no content")
		return nil
	}
	values := []gjson.Result{output}
	if output.IsArray() {
		values = output.Array()
	}
	parts := make([][]byte, 0, len(values))
	for _, value := range values {
		text := value.String()
		if value.IsObject() {
			if partText := value.Get("text"); partText.Type == gjson.String {
				text = partText.String()
			} else {
				text = value.Raw
			}
		} else if value.IsArray() {
			text = value.Raw
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		part := []byte(`{"text":""}`)
		part, _ = sjson.SetBytes(part, "text", text)
		parts = append(parts, part)
	}
	return parts
}

func buildOpenAIResponsesFunctionResponseParts(
	item gjson.Result,
	functionNamesByCallID map[string]string,
) ([][]byte, error) {
	callID := translatorcommon.ExtractResponsesCallID(item)
	functionName := openAIResponsesGeminiToolName(item)
	if matchedName, ok := functionNamesByCallID[callID]; ok {
		functionName = matchedName
	}
	if functionName == "" {
		functionName = "unknown"
	}
	functionResponse := []byte(`{"functionResponse":{"name":"","response":{}}}`)
	functionResponse, _ = sjson.SetBytes(functionResponse, "functionResponse.name", functionName)
	functionResponse, _ = sjson.SetBytes(functionResponse, "functionResponse.id", callID)

	output := item.Get("output")
	if output.Type == gjson.String {
		value := output.String()
		if value != "" && value != "null" {
			functionResponse, _ = sjson.SetBytes(functionResponse, "functionResponse.response.result", value)
		}
		return [][]byte{functionResponse}, nil
	}

	var imageParts [][]byte
	switch {
	case output.IsArray():
		result, rawResult, images, errMedia := parseOpenAIResponsesArrayOutput(output)
		if errMedia != nil {
			return nil, errMedia
		}
		imageParts = images
		if rawResult {
			functionResponse = translatorcommon.SetGeminiFunctionResponseRaw(
				functionResponse, "functionResponse.response.result", result,
			)
		} else {
			functionResponse, _ = sjson.SetBytes(functionResponse, "functionResponse.response.result", result)
		}
	case output.IsObject():
		part, errMedia := openAIResponsesPartFromBlock(output)
		if errMedia != nil {
			return nil, errMedia
		}
		if len(part) > 0 {
			imageParts = append(imageParts, part)
			functionResponse, _ = sjson.SetBytes(functionResponse, "functionResponse.response.result", "")
		} else {
			functionResponse = translatorcommon.SetGeminiFunctionResponseResult(
				functionResponse, "functionResponse.response.result", output,
			)
		}
	case output.Raw != "" && output.Raw != "null":
		functionResponse, _ = sjson.SetBytes(
			functionResponse, "functionResponse.response.result", output.String(),
		)
	}

	for _, mediaPart := range imageParts {
		if inline := gjson.GetBytes(mediaPart, "inline_data"); inline.Exists() {
			inlineData := []byte(`{"inlineData":{"mimeType":"","data":""}}`)
			inlineData, _ = sjson.SetBytes(inlineData, "inlineData.mimeType", inline.Get("mime_type").String())
			inlineData, _ = sjson.SetBytes(inlineData, "inlineData.data", inline.Get("data").String())
			functionResponse, _ = sjson.SetRawBytes(functionResponse, "functionResponse.parts.-1", inlineData)
		} else if fileData := gjson.GetBytes(mediaPart, "file_data"); fileData.Exists() {
			filePart := []byte(`{"fileData":{"mimeType":"","fileUri":""}}`)
			filePart, _ = sjson.SetBytes(filePart, "fileData.mimeType", fileData.Get("mime_type").String())
			filePart, _ = sjson.SetBytes(filePart, "fileData.fileUri", fileData.Get("file_uri").String())
			functionResponse, _ = sjson.SetRawBytes(functionResponse, "functionResponse.parts.-1", filePart)
		}
	}
	return [][]byte{functionResponse}, nil
}

func buildOpenAIResponsesSynthesizedFunctionResponsePart(
	callID string,
	functionNamesByCallID map[string]string,
) []byte {
	functionName := "unknown"
	if matchedName := functionNamesByCallID[callID]; matchedName != "" {
		functionName = matchedName
	}
	functionResponse := []byte(`{"functionResponse":{"name":"","response":{"result":"call interrupted, no output"}}}`)
	functionResponse, _ = sjson.SetBytes(functionResponse, "functionResponse.name", functionName)
	if callID != "" {
		functionResponse, _ = sjson.SetBytes(functionResponse, "functionResponse.id", callID)
	}
	return functionResponse
}

func responsesHasMatchingOutput(items []gjson.Result, callID string) bool {
	if callID == "" {
		return false
	}
	for _, item := range items {
		if isOpenAIResponsesToolOutput(item) && translatorcommon.ExtractResponsesCallID(item) == callID {
			return true
		}
	}
	return false
}

func collectOpenAIResponsesFunctionCallOutputs(
	items []gjson.Result,
	start int,
	pendingCallIDs []string,
) ([]gjson.Result, map[int]bool, []string) {
	end := start + 1
	for end < len(items) && isOpenAIResponsesToolOutput(items[end]) {
		end++
	}
	outputs := items[start:end]
	ordered, remainingPending := orderOpenAIResponsesFunctionCallOutputs(outputs, pendingCallIDs)
	consumed := make(map[int]bool, len(outputs))
	for itemIndex := start; itemIndex < end; itemIndex++ {
		consumed[itemIndex] = true
	}
	return ordered, consumed, remainingPending
}

func orderOpenAIResponsesFunctionCallOutputs(outputs []gjson.Result, pendingCallIDs []string) (
	[]gjson.Result,
	[]string,
) {
	ordered := make([]gjson.Result, 0, len(outputs))
	used := make([]bool, len(outputs))
	remainingPending := make([]string, 0, len(pendingCallIDs))
	for _, pendingID := range pendingCallIDs {
		match := -1
		for outputIndex, output := range outputs {
			if !used[outputIndex] && translatorcommon.ExtractResponsesCallID(output) == pendingID {
				match = outputIndex
				break
			}
		}
		if match < 0 {
			remainingPending = append(remainingPending, pendingID)
			continue
		}
		used[match] = true
		ordered = append(ordered, outputs[match])
	}
	for outputIndex, output := range outputs {
		if !used[outputIndex] {
			ordered = append(ordered, output)
		}
	}
	return ordered, remainingPending
}

func buildOpenAIResponsesFunctionCallModelContent(item gjson.Result, signature string) []byte {
	modelContent := []byte(`{"role":"model","parts":[]}`)
	modelContent, _ = sjson.SetRawBytes(
		modelContent, "parts",
		translatorcommon.JoinRawArray([][]byte{buildOpenAIResponsesFunctionCallPart(item, signature)}),
	)
	return modelContent
}

func buildOpenAIResponsesEmptyReasoningFunctionCallModelContent(item gjson.Result, signature string) []byte {
	thought := []byte(`{"text":"","thought":true,"thoughtSignature":""}`)
	thought, _ = sjson.SetBytes(thought, "thoughtSignature", signature)
	parts := [][]byte{thought, buildOpenAIResponsesFunctionCallPart(item, signature)}
	modelContent := []byte(`{"role":"model","parts":[]}`)
	modelContent, _ = sjson.SetRawBytes(modelContent, "parts", translatorcommon.JoinRawArray(parts))
	return modelContent
}

func buildOpenAIResponsesReasoningFunctionCallModelContent(
	thoughtText string,
	item gjson.Result,
	signature string,
) []byte {
	parts := make([][]byte, 0, 2)
	if thoughtText != "" {
		thought := []byte(`{"text":"","thought":true}`)
		thought, _ = sjson.SetBytes(thought, "text", thoughtText)
		parts = append(parts, thought)
	}
	parts = append(parts, buildOpenAIResponsesFunctionCallPart(item, signature))
	modelContent := []byte(`{"role":"model","parts":[]}`)
	modelContent, _ = sjson.SetRawBytes(modelContent, "parts", translatorcommon.JoinRawArray(parts))
	return modelContent
}

func buildOpenAIResponsesReasoningModelContent(
	thoughtText, visibleText, signature string,
	useGeminiNativeReasoningLayout bool,
) []byte {
	modelContent := []byte(`{"role":"model","parts":[]}`)
	if useGeminiNativeReasoningLayout {
		if thoughtText == "" && visibleText == "" {
			carrier := []byte(`{"text":"","thoughtSignature":""}`)
			carrier, _ = sjson.SetBytes(carrier, "thoughtSignature", signature)
			modelContent, _ = sjson.SetRawBytes(modelContent, "parts.-1", carrier)
			return modelContent
		}
		if thoughtText != "" {
			thought := []byte(`{"text":"","thought":true}`)
			thought, _ = sjson.SetBytes(thought, "text", thoughtText)
			if visibleText == "" {
				thought, _ = sjson.SetBytes(thought, "thoughtSignature", signature)
			}
			modelContent, _ = sjson.SetRawBytes(modelContent, "parts.-1", thought)
		}
		if visibleText != "" {
			visible := []byte(`{"text":"","thoughtSignature":""}`)
			visible, _ = sjson.SetBytes(visible, "text", visibleText)
			visible, _ = sjson.SetBytes(visible, "thoughtSignature", signature)
			modelContent, _ = sjson.SetRawBytes(modelContent, "parts.-1", visible)
		}
		return modelContent
	}

	thought := []byte(`{"text":"","thoughtSignature":"","thought":true}`)
	thought, _ = sjson.SetBytes(thought, "text", thoughtText)
	thought, _ = sjson.SetBytes(thought, "thoughtSignature", signature)
	modelContent, _ = sjson.SetRawBytes(modelContent, "parts.-1", thought)
	return modelContent
}

func openAIResponsesGeminiThoughtSignature(rawSignature string) string {
	return sigcompat.GeminiReplaySignatureOrBypass(rawSignature, sigcompat.BlockKindGeminiModelPart)
}

func applyOpenAIResponsesTextFormatToGemini(out []byte, root gjson.Result) []byte {
	textFormat := root.Get("text.format")
	if !textFormat.Exists() {
		return out
	}

	formatType := strings.ToLower(strings.TrimSpace(textFormat.Get("type").String()))
	switch formatType {
	case "json_object":
		out, _ = sjson.SetBytes(out, "generationConfig.responseMimeType", "application/json")
	case "json_schema":
		out, _ = sjson.SetBytes(out, "generationConfig.responseMimeType", "application/json")

		schema := textFormat.Get("schema")
		if !schema.Exists() {
			schema = textFormat.Get("json_schema.schema")
		}
		if schema.Exists() {
			out, _ = sjson.SetRawBytes(out, "generationConfig.responseJsonSchema", []byte(schema.Raw))
		}
	}

	return out
}
