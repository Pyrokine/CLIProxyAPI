// Package claude provides request translation functionality for Claude Code API compatibility.
// This package handles the conversion of Claude Code API requests into Gemini CLI-compatible
// JSON format, transforming message contents, system instructions, and tool declarations
// into the format expected by Gemini CLI API clients. It performs JSON data transformation
// to ensure compatibility between Claude Code API format and Gemini CLI API's expected format.
package claude

import (
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/cache"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/signature"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/gemini/common"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func resolveThinkingSignature(modelName, thinkingText, rawSignature string) string {
	targetProvider := signature.ProviderFromModelName(modelName)
	if strings.TrimSpace(rawSignature) == "" && thinkingText != "" {
		rawSignature = cache.GetCachedSignature(modelName, thinkingText)
	}
	if strings.TrimSpace(rawSignature) == "" {
		return ""
	}

	switch targetProvider {
	case signature.ProviderGemini:
		innerSignature, _, targetKind, marked, ok := decodeGeminiClaudeCarrierSignature(rawSignature)
		if !ok {
			return ""
		}
		blockKind := signature.BlockKindGeminiModelPart
		if marked && targetKind == geminiClaudeCarrierFunction {
			blockKind = signature.BlockKindGeminiFunctionCall
		}
		return signature.GeminiReplaySignatureOrBypass(innerSignature, blockKind)
	case signature.ProviderClaude:
		normalized, compatible := signature.CompatibleAntigravityClaudeThinkingSignature(rawSignature)
		if compatible {
			return normalized
		}
	default:
		normalized, compatible := signature.CompatibleSignatureForProvider(
			targetProvider,
			rawSignature,
			signature.BlockKindClaudeThinking,
		)
		if compatible {
			return normalized
		}
	}
	return ""
}

func hasResolvedThinkingSignature(signature string) bool {
	return signature != ""
}

func resolveToolUseThoughtSignature(modelName string, contentResult gjson.Result) string {
	targetProvider := signature.ProviderFromModelName(modelName)
	for _, path := range []string{
		"signature",
		"thought_signature",
		"extra_content.google.thought_signature",
	} {
		signatureResult := contentResult.Get(path)
		if !signatureResult.Exists() {
			continue
		}
		normalized, compatible := signature.CompatibleSignatureForProvider(
			targetProvider,
			signatureResult.String(),
			signature.BlockKindGeminiFunctionCall,
		)
		if compatible {
			return normalized
		}
	}
	if targetProvider == signature.ProviderGemini {
		return signature.GeminiSkipThoughtSignatureValidator
	}
	return ""
}

func antigravityClaudeContent(role string, parts [][]byte) []byte {
	content := []byte(`{"role":"","parts":[]}`)
	content, _ = sjson.SetBytes(content, "role", role)
	content, _ = sjson.SetRawBytes(content, "parts", translatorcommon.JoinRawArray(parts))
	return content
}

func mapClaudeFunctionNameToAntigravity(functionNameMap map[string]string, name string) string {
	return translatorcommon.AntigravityToolNameToUpstream(
		util.MapSanitizedFunctionName(functionNameMap, name),
	)
}

// convertClaudeRequestToAntigravity converts a Claude request to the Gemini CLI request schema.
// It maps system instructions, messages, tools, and generation settings; stream is currently unused.
func convertClaudeRequestToAntigravity(modelName string, inputRawJSON []byte, stream bool) []byte {
	out, _ := convertClaudeRequestToAntigravityChecked(modelName, inputRawJSON, stream)
	return out
}

func convertClaudeRequestToAntigravityChecked(modelName string, inputRawJSON []byte, _ bool) ([]byte, error) {
	enableThoughtTranslate := true
	var drops translatorcommon.UserTurnDrops
	rawJSON := inputRawJSON
	if shouldBuildAntigravityWebSearchRequest(modelName, rawJSON) {
		for _, message := range gjson.GetBytes(rawJSON, "messages").Array() {
			if message.Get("role").String() != "user" {
				continue
			}
			sendable := 0
			for _, part := range message.Get("content").Array() {
				switch part.Get("type").String() {
				case "text":
					if part.Get("text").String() != "" {
						sendable++
					}
				case "image", "document", "container_upload", "input_audio":
					drops.Drop(part.Get("type").String())
				}
			}
			drops.EndTurn(sendable)
		}
		if err := drops.Err(); err != nil {
			return nil, err
		}
		return buildAntigravityWebSearchRequest(modelName, rawJSON), nil
	}
	functionNameMap := util.SanitizedFunctionNameMap(rawJSON)

	// system instruction
	var systemInstructionJSON []byte
	hasSystemInstruction := false
	systemResult := gjson.GetBytes(rawJSON, "system")
	if systemResult.IsArray() {
		systemResults := systemResult.Array()
		systemInstructionJSON = []byte(`{"role":"user","parts":[]}`)
		for i := range systemResults {
			systemPromptResult := systemResults[i]
			systemTypePromptResult := systemPromptResult.Get("type")
			if systemTypePromptResult.Type == gjson.String && systemTypePromptResult.String() == "text" {
				systemPrompt := systemPromptResult.Get("text").String()
				if util.IsClaudeCodeAttributionSystemText(systemPrompt) {
					continue
				}
				partJSON := []byte(`{}`)
				if systemPrompt != "" {
					partJSON, _ = sjson.SetBytes(partJSON, "text", systemPrompt)
				}
				systemInstructionJSON, _ = sjson.SetRawBytes(systemInstructionJSON, "parts.-1", partJSON)
				hasSystemInstruction = true
			}
		}
	} else if systemResult.Type == gjson.String && !util.IsClaudeCodeAttributionSystemText(systemResult.String()) {
		systemInstructionJSON = []byte(`{"role":"user","parts":[{"text":""}]}`)
		systemInstructionJSON, _ = sjson.SetBytes(systemInstructionJSON, "parts.0.text", systemResult.String())
		hasSystemInstruction = true
	}

	// contents
	contentsJSON := []byte(`[]`)
	hasContents := false

	// tool_use_id → tool_name lookup, populated incrementally during the main loop.
	// Claude's tool_result references tool_use by ID; Gemini requires functionResponse.name.
	toolNameByID := make(map[string]string)
	var pendingToolUseIDs []string

	messagesResult := gjson.GetBytes(rawJSON, "messages")
	if messagesResult.IsArray() {
		messageResults := messagesResult.Array()
		numMessages := len(messageResults)
		for i := range numMessages {
			messageResult := messageResults[i]
			roleResult := messageResult.Get("role")
			if roleResult.Type != gjson.String {
				continue
			}
			originalRole := roleResult.String()
			var precedingToolUseIDs []string
			if originalRole != "system" && originalRole != "developer" {
				precedingToolUseIDs = pendingToolUseIDs
				pendingToolUseIDs = nil
			}
			role := originalRole
			if role == "assistant" {
				role = "model"
			} else if role == "system" || role == "developer" {
				role = "user"
			}
			partItems := make([][]byte, 0, 4)
			appendDetachedCarrier := func(rawSignature string) {
				carrier := []byte(`{"text":"","thoughtSignature":""}`)
				carrier, _ = sjson.SetBytes(carrier, "thoughtSignature", rawSignature)
				partItems = append(partItems, carrier)
			}
			pendingDetachedSignature := ""
			pendingDetachedTargetKind := ""
			clearPendingDetachedSignature := func() {
				pendingDetachedSignature = ""
				pendingDetachedTargetKind = ""
			}
			setPendingDetachedSignature := func(rawSignature, targetKind string) {
				if pendingDetachedSignature != "" {
					appendDetachedCarrier(pendingDetachedSignature)
				}
				pendingDetachedSignature = rawSignature
				pendingDetachedTargetKind = targetKind
			}
			contentsResult := messageResult.Get("content")
			if originalRole == "system" || originalRole == "developer" {
				if reminderText, ok := translatorcommon.ClaudeMessageSystemReminderText(contentsResult); ok {
					partJSON := []byte(`{}`)
					partJSON, _ = sjson.SetBytes(partJSON, "text", reminderText)
					partItems = append(partItems, partJSON)
					contentsJSON, _ = sjson.SetRawBytes(
						contentsJSON,
						"-1",
						antigravityClaudeContent(role, partItems),
					)
					hasContents = true
				}
				continue
			}
			if contentsResult.IsArray() {
				if originalRole == "user" {
					contentResults := translatorcommon.DeduplicateToolResultsByRawID(
						contentsResult.Array(),
						func(content gjson.Result) (string, bool) {
							if content.Get("type").String() != "tool_result" {
								return "", false
							}
							return content.Get("tool_use_id").String(), true
						},
					)
					rawContents := make([][]byte, len(contentResults))
					for contentIndex, content := range contentResults {
						rawContents[contentIndex] = []byte(content.Raw)
					}
					contentsResult = gjson.ParseBytes(translatorcommon.JoinRawArray(rawContents))
					contentsResult = translatorcommon.AlignClaudeToolResults(contentsResult, precedingToolUseIDs)
				}
				contentResults := contentsResult.Array()
				numContents := len(contentResults)
				var currentMessageThinkingSignature string
				for j := range numContents {
					contentResult := contentResults[j]
					contentTypeResult := contentResult.Get("type")
					if contentTypeResult.Type == gjson.String && contentTypeResult.String() == "thinking" {
						if originalRole != "assistant" {
							continue
						}
						thinkingText := thinking.GetThinkingText(contentResult)
						signatureResult := contentResult.Get("signature")
						resolvedSignature := resolveThinkingSignature(
							modelName,
							thinkingText,
							signatureResult.String(),
						)
						if resolvedSignature != "" && pendingDetachedSignature != "" {
							if pendingDetachedSignature != resolvedSignature {
								appendDetachedCarrier(pendingDetachedSignature)
							}
							clearPendingDetachedSignature()
						}
						signatureFromPendingCarrier := false
						if resolvedSignature == "" && thinkingText != "" && pendingDetachedSignature != "" {
							if pendingDetachedTargetKind == "" ||
								pendingDetachedTargetKind == geminiClaudeCarrierAny ||
								pendingDetachedTargetKind == geminiClaudeCarrierText {
								resolvedSignature = pendingDetachedSignature
								signatureFromPendingCarrier = true
							} else {
								appendDetachedCarrier(pendingDetachedSignature)
							}
							clearPendingDetachedSignature()
						}
						if !hasResolvedThinkingSignature(resolvedSignature) {
							enableThoughtTranslate = false
							continue
						}

						nextAcceptsDetachedSignature := false
						nextTargetKind := geminiClaudeCarrierAny
						if j+1 < numContents {
							switch contentResults[j+1].Get("type").String() {
							case "text":
								nextAcceptsDetachedSignature = true
								nextTargetKind = geminiClaudeCarrierText
							case "tool_use":
								nextAcceptsDetachedSignature = true
								nextTargetKind = geminiClaudeCarrierFunction
							}
						}
						isGeminiSignature := signature.ProviderFromModelName(modelName) == signature.ProviderGemini
						_, carrierDirection, carrierTargetKind, markedCarrier, validCarrier :=
							decodeGeminiClaudeCarrierSignature(signatureResult.String())

						if thinkingText != "" {
							partJSON := []byte(`{}`)
							partJSON, _ = sjson.SetBytes(partJSON, "thought", true)
							partJSON, _ = sjson.SetBytes(partJSON, "text", thinkingText)
							if signatureFromPendingCarrier {
								partJSON, _ = sjson.SetBytes(partJSON, "thoughtSignature", resolvedSignature)
							} else if markedCarrier {
								carrierTargetsNext := carrierTargetKind == geminiClaudeCarrierAny ||
									carrierTargetKind == nextTargetKind
								if validCarrier && carrierDirection == geminiClaudeCarrierStandalone &&
									(carrierTargetKind == geminiClaudeCarrierText ||
										carrierTargetKind == geminiClaudeCarrierAny) {
									partJSON, _ = sjson.SetBytes(partJSON, "thoughtSignature", resolvedSignature)
								} else if validCarrier && carrierDirection == geminiClaudeCarrierNext &&
									nextAcceptsDetachedSignature && carrierTargetsNext {
									setPendingDetachedSignature(resolvedSignature, carrierTargetKind)
								}
							} else if isGeminiSignature && nextAcceptsDetachedSignature {
								setPendingDetachedSignature(resolvedSignature, nextTargetKind)
							} else {
								partJSON, _ = sjson.SetBytes(partJSON, "thoughtSignature", resolvedSignature)
								currentMessageThinkingSignature = resolvedSignature
							}
							partItems = append(partItems, partJSON)
							continue
						}

						if !isGeminiSignature || markedCarrier && !validCarrier {
							continue
						}
						if markedCarrier && carrierDirection == geminiClaudeCarrierNext {
							if geminiClaudeCarrierMatchesAdjacent(
								contentResults,
								j,
								carrierDirection,
								carrierTargetKind,
							) {
								setPendingDetachedSignature(resolvedSignature, carrierTargetKind)
							}
							continue
						}
						if markedCarrier && carrierDirection == geminiClaudeCarrierStandalone {
							appendDetachedCarrier(resolvedSignature)
							continue
						}

						bindBackward := markedCarrier && carrierDirection == geminiClaudeCarrierPrevious
						if bindBackward && !geminiClaudeCarrierMatchesAdjacent(
							contentResults,
							j,
							carrierDirection,
							carrierTargetKind,
						) {
							continue
						}
						if !bindBackward && nextAcceptsDetachedSignature {
							setPendingDetachedSignature(resolvedSignature, nextTargetKind)
							continue
						}
						attached := false
						foundSemanticPart := false
						for partIndex := len(partItems) - 1; partIndex >= 0; partIndex-- {
							part := gjson.ParseBytes(partItems[partIndex])
							partTargetKind := ""
							switch {
							case part.Get("functionCall").Exists():
								partTargetKind = geminiClaudeCarrierFunction
							case part.Get("text").Exists() && part.Get("text").String() != "":
								partTargetKind = geminiClaudeCarrierText
							default:
								continue
							}
							foundSemanticPart = true
							if markedCarrier && carrierTargetKind != geminiClaudeCarrierAny &&
								carrierTargetKind != partTargetKind {
								break
							}
							partSignature := strings.TrimSpace(part.Get("thoughtSignature").String())
							replaceFallback := bindBackward &&
								partTargetKind == geminiClaudeCarrierFunction &&
								partSignature == signature.GeminiSkipThoughtSignatureValidator
							if partSignature == "" || replaceFallback {
								partItems[partIndex], _ = sjson.SetBytes(
									partItems[partIndex],
									"thoughtSignature",
									resolvedSignature,
								)
								attached = true
							}
							break
						}
						if !attached && (foundSemanticPart || bindBackward) {
							appendDetachedCarrier(resolvedSignature)
						} else if !attached {
							setPendingDetachedSignature(resolvedSignature, carrierTargetKind)
						}
					} else if contentTypeResult.Type == gjson.String && contentTypeResult.String() == "text" {
						prompt := contentResult.Get("text").String()
						// Skip empty text parts to avoid Gemini API error:
						// "required oneof field 'data' must have one initialized field"
						if prompt == "" {
							continue
						}
						partJSON := []byte(`{}`)
						partJSON, _ = sjson.SetBytes(partJSON, "text", prompt)
						if pendingDetachedSignature != "" {
							if pendingDetachedTargetKind == "" ||
								pendingDetachedTargetKind == geminiClaudeCarrierAny ||
								pendingDetachedTargetKind == geminiClaudeCarrierText {
								partJSON, _ = sjson.SetBytes(
									partJSON,
									"thoughtSignature",
									pendingDetachedSignature,
								)
							} else {
								appendDetachedCarrier(pendingDetachedSignature)
							}
							clearPendingDetachedSignature()
						}
						partItems = append(partItems, partJSON)
					} else if contentTypeResult.Type == gjson.String && contentTypeResult.String() == "tool_use" {
						// NOTE: Do NOT inject dummy thinking blocks here.
						// Antigravity API validates signatures, so dummy values are rejected.

						originalFunctionName := contentResult.Get("name").String()
						functionName := mapClaudeFunctionNameToAntigravity(functionNameMap, originalFunctionName)
						argsResult := contentResult.Get("input")
						functionID := contentResult.Get("id").String()

						if functionID != "" && originalFunctionName != "" {
							toolNameByID[functionID] = originalFunctionName
						}

						// Preserve every present input as valid JSON for the function call.
						var argsRaw string
						if argsResult.IsObject() {
							argsRaw = argsResult.Raw
						} else if argsResult.Exists() {
							switch argsResult.Type {
							case gjson.String:
								// Parse JSON-encoded object strings while preserving other strings as JSON strings.
								parsed := gjson.Parse(argsResult.String())
								if parsed.IsObject() {
									argsRaw = parsed.Raw
								} else {
									argsRaw = argsResult.Raw
								}
							case gjson.Null:
								argsRaw = `{}`
							default:
								argsRaw = argsResult.Raw
							}
						}

						if argsRaw != "" {
							partJSON := []byte(`{}`)

							toolSignature := resolveToolUseThoughtSignature(modelName, contentResult)
							if toolSignature == "" {
								toolSignature = currentMessageThinkingSignature
							}
							if pendingDetachedSignature != "" {
								pendingMatchesTool := pendingDetachedTargetKind == "" ||
									pendingDetachedTargetKind == geminiClaudeCarrierAny ||
									pendingDetachedTargetKind == geminiClaudeCarrierFunction
								if pendingMatchesTool &&
									(toolSignature == "" ||
										toolSignature == signature.GeminiSkipThoughtSignatureValidator) {
									toolSignature = pendingDetachedSignature
								} else {
									appendDetachedCarrier(pendingDetachedSignature)
								}
								clearPendingDetachedSignature()
							}
							if hasResolvedThinkingSignature(toolSignature) {
								partJSON, _ = sjson.SetBytes(partJSON, "thoughtSignature", toolSignature)
							} else if signature.ProviderFromModelName(modelName) == signature.ProviderGemini {
								partJSON, _ = sjson.SetBytes(
									partJSON, "thoughtSignature", signature.GeminiSkipThoughtSignatureValidator,
								)
							}

							if functionID != "" {
								partJSON, _ = sjson.SetBytes(partJSON, "functionCall.id", functionID)
							}
							partJSON, _ = sjson.SetBytes(partJSON, "functionCall.name", functionName)
							partJSON, _ = sjson.SetRawBytes(partJSON, "functionCall.args", []byte(argsRaw))
							partItems = append(partItems, partJSON)
							if originalRole == "assistant" && functionID != "" {
								pendingToolUseIDs = append(pendingToolUseIDs, functionID)
							}
						}
					} else if contentTypeResult.Type == gjson.String && contentTypeResult.String() == "tool_result" {
						toolCallID := contentResult.Get("tool_use_id").String()
						if toolCallID != "" {
							funcName, ok := toolNameByID[toolCallID]
							if !ok {
								// Fallback: derive a semantic name from the ID by stripping
								// the last two dash-separated segments (e.g. "get_weather-call-123" → "get_weather").
								// Only use the raw ID as a last resort when the heuristic produces an empty string.
								parts := strings.Split(toolCallID, "-")
								if len(parts) > 2 {
									funcName = strings.Join(parts[:len(parts)-2], "-")
								}
								if funcName == "" {
									funcName = toolCallID
								}
								log.Warnf(
									"antigravity claude request: tool_result references unknown tool_use_id=%s, derived function name=%s",
									toolCallID, funcName,
								)
							}
							functionResponseResult := contentResult.Get("content")

							functionResponseJSON := []byte(`{}`)
							functionResponseJSON, _ = sjson.SetBytes(functionResponseJSON, "id", toolCallID)
							functionResponseJSON, _ = sjson.SetBytes(
								functionResponseJSON,
								"name",
								mapClaudeFunctionNameToAntigravity(functionNameMap, funcName),
							)

							responseData := ""
							if functionResponseResult.Type == gjson.String {
								responseData = functionResponseResult.String()
								functionResponseJSON, _ = sjson.SetBytes(
									functionResponseJSON, "response.result", responseData,
								)
							} else if functionResponseResult.IsArray() {
								frResults := functionResponseResult.Array()
								nonImageCount := 0
								lastNonImageRaw := ""
								filteredJSON := []byte(`[]`)
								imagePartsJSON := []byte(`[]`)
								for _, fr := range frResults {
									if fr.Get("type").String() == "image" && fr.Get("source.type").String() == "base64" {
										inlineDataJSON := []byte(`{}`)
										if mimeType := fr.Get("source.media_type").String(); mimeType != "" {
											inlineDataJSON, _ = sjson.SetBytes(inlineDataJSON, "mimeType", mimeType)
										}
										if data := fr.Get("source.data").String(); data != "" {
											inlineDataJSON, _ = sjson.SetBytes(inlineDataJSON, "data", data)
										}

										imagePartJSON := []byte(`{}`)
										imagePartJSON, _ = sjson.SetRawBytes(
											imagePartJSON, "inlineData", inlineDataJSON,
										)
										imagePartsJSON, _ = sjson.SetRawBytes(imagePartsJSON, "-1", imagePartJSON)
										continue
									}

									nonImageCount++
									lastNonImageRaw = fr.Raw
									filteredJSON, _ = sjson.SetRawBytes(filteredJSON, "-1", []byte(fr.Raw))
								}

								if nonImageCount == 1 {
									functionResponseJSON = translatorcommon.SetGeminiFunctionResponseRaw(
										functionResponseJSON, "response.result", lastNonImageRaw,
									)
								} else if nonImageCount > 1 {
									functionResponseJSON = translatorcommon.SetGeminiFunctionResponseRaw(
										functionResponseJSON, "response.result", string(filteredJSON),
									)
								} else {
									functionResponseJSON, _ = sjson.SetBytes(
										functionResponseJSON, "response.result", "",
									)
								}

								// Place image data inside functionResponse.parts as inlineData
								// instead of as sibling parts in the outer content, to avoid
								// base64 data bloating the text context.
								if gjson.GetBytes(imagePartsJSON, "#").Int() > 0 {
									functionResponseJSON, _ = sjson.SetRawBytes(
										functionResponseJSON, "parts", imagePartsJSON,
									)
								}

							} else if functionResponseResult.IsObject() {
								if functionResponseResult.Get("type").String() == "image" && functionResponseResult.Get("source.type").String() == "base64" {
									inlineDataJSON := []byte(`{}`)
									if mimeType := functionResponseResult.Get("source.media_type").String(); mimeType != "" {
										inlineDataJSON, _ = sjson.SetBytes(inlineDataJSON, "mimeType", mimeType)
									}
									if data := functionResponseResult.Get("source.data").String(); data != "" {
										inlineDataJSON, _ = sjson.SetBytes(inlineDataJSON, "data", data)
									}

									imagePartJSON := []byte(`{}`)
									imagePartJSON, _ = sjson.SetRawBytes(imagePartJSON, "inlineData", inlineDataJSON)
									imagePartsJSON := []byte(`[]`)
									imagePartsJSON, _ = sjson.SetRawBytes(imagePartsJSON, "-1", imagePartJSON)
									functionResponseJSON, _ = sjson.SetRawBytes(
										functionResponseJSON, "parts", imagePartsJSON,
									)
									functionResponseJSON, _ = sjson.SetBytes(
										functionResponseJSON, "response.result", "",
									)
								} else {
									functionResponseJSON = translatorcommon.SetGeminiFunctionResponseResult(
										functionResponseJSON, "response.result", functionResponseResult,
									)
								}
							} else if functionResponseResult.Raw != "" {
								functionResponseJSON = translatorcommon.SetGeminiFunctionResponseResult(
									functionResponseJSON, "response.result", functionResponseResult,
								)
							} else {
								// A missing content field leaves Raw empty, which causes
								// sjson.SetRaw to assign an invalid JSON value to result.
								functionResponseJSON, _ = sjson.SetBytes(functionResponseJSON, "response.result", "")
							}

							partJSON := []byte(`{}`)
							partJSON, _ = sjson.SetRawBytes(partJSON, "functionResponse", functionResponseJSON)
							partItems = append(partItems, partJSON)
						}
					} else if contentTypeResult.Type == gjson.String &&
						(contentTypeResult.String() == "image" || contentTypeResult.String() == "document" ||
							contentTypeResult.String() == "container_upload") {
						sourceResult := contentResult.Get("source")
						mimeType := sourceResult.Get("media_type").String()
						data := sourceResult.Get("data").String()
						if sourceResult.Get("type").String() == "base64" && mimeType != "" && data != "" {
							partJSON := []byte(`{"inlineData":{"mimeType":"","data":""}}`)
							partJSON, _ = sjson.SetBytes(partJSON, "inlineData.mimeType", mimeType)
							partJSON, _ = sjson.SetBytes(partJSON, "inlineData.data", data)
							partItems = append(partItems, partJSON)
						} else if originalRole == "user" {
							drops.Drop(contentTypeResult.String())
						}
					} else if contentTypeResult.String() == "input_audio" && originalRole == "user" {
						drops.Drop("input_audio")
					}
				}

				if pendingDetachedSignature != "" {
					appendDetachedCarrier(pendingDetachedSignature)
					clearPendingDetachedSignature()
				}

				if originalRole == "user" {
					drops.EndTurn(translatorcommon.CountSendableGeminiParts(partItems))
				}
				if len(partItems) == 0 {
					continue
				}
				if role == "model" && len(partItems) > 1 {
					thinkingParts := make([][]byte, 0, len(partItems))
					regularParts := make([][]byte, 0, len(partItems))
					trailingParts := make([][]byte, 0, len(partItems))
					needsReorder := false
					previousCategory := -1
					seenFunctionCall := false
					for _, partJSON := range partItems {
						part := gjson.ParseBytes(partJSON)
						category := 1
						isSignatureCarrier := part.Get("text").Exists() &&
							part.Get("text").String() == "" &&
							strings.TrimSpace(part.Get("thoughtSignature").String()) != ""
						isFunctionTailCarrier := isSignatureCarrier && seenFunctionCall
						switch {
						case part.Get("thought").Bool():
							category = 0
							thinkingParts = append(thinkingParts, partJSON)
						case part.Get("functionCall").Exists() || isFunctionTailCarrier:
							category = 2
							trailingParts = append(trailingParts, partJSON)
							seenFunctionCall = seenFunctionCall || part.Get("functionCall").Exists()
						default:
							regularParts = append(regularParts, partJSON)
						}
						needsReorder = needsReorder || category < previousCategory
						previousCategory = category
					}
					if needsReorder {
						partItems = append(thinkingParts, regularParts...)
						partItems = append(partItems, trailingParts...)
					}
				}
				if role == "user" {
					partItems = translatorcommon.ReorderGeminiUserParts(partItems)
				}

				contentsJSON, _ = sjson.SetRawBytes(
					contentsJSON,
					"-1",
					antigravityClaudeContent(role, partItems),
				)
				hasContents = true
			} else if contentsResult.Type == gjson.String {
				prompt := contentsResult.String()
				partJSON := []byte(`{}`)
				if prompt != "" {
					partJSON, _ = sjson.SetBytes(partJSON, "text", prompt)
				}
				partItems = append(partItems, partJSON)
				contentsJSON, _ = sjson.SetRawBytes(
					contentsJSON,
					"-1",
					antigravityClaudeContent(role, partItems),
				)
				hasContents = true
			}
		}
	}

	// tools
	var toolsJSON []byte
	toolDeclCount := 0
	allowedToolKeys := []string{
		"name", "description", "behavior", "parameters", "parametersJsonSchema", "response", "responseJsonSchema",
	}
	toolsResult := gjson.GetBytes(rawJSON, "tools")
	if toolsResult.IsArray() {
		functionToolNode := []byte(`{"functionDeclarations":[]}`)
		toolsResults := toolsResult.Array()
		for i := range toolsResults {
			toolResult := toolsResults[i]
			if isClaudeTypedWebSearchToolType(toolResult.Get("type").String()) {
				continue
			}
			inputSchemaResult := toolResult.Get("input_schema")
			if inputSchemaResult.Exists() && inputSchemaResult.IsObject() {
				// Sanitize the input schema for Antigravity API compatibility
				inputSchema := util.CleanJSONSchemaForAntigravity(inputSchemaResult.Raw)
				tool, _ := sjson.DeleteBytes([]byte(toolResult.Raw), "input_schema")
				tool, _ = sjson.SetRawBytes(tool, "parametersJsonSchema", []byte(inputSchema))
				tool, _ = sjson.SetBytes(
					tool,
					"name",
					mapClaudeFunctionNameToAntigravity(functionNameMap, gjson.GetBytes(tool, "name").String()),
				)
				for toolKey := range gjson.ParseBytes(tool).Map() {
					if util.InArray(allowedToolKeys, toolKey) {
						continue
					}
					tool, _ = sjson.DeleteBytes(tool, toolKey)
				}
				functionToolNode, _ = sjson.SetRawBytes(functionToolNode, "functionDeclarations.-1", tool)
				toolDeclCount++
			}
		}
		if toolDeclCount > 0 {
			declarations := gjson.GetBytes(functionToolNode, "functionDeclarations")
			deduplicated := util.DeduplicateFunctionDeclarations([]byte(declarations.Raw))
			functionToolNode, _ = sjson.SetRawBytes(functionToolNode, "functionDeclarations", deduplicated)
			toolDeclCount = len(gjson.ParseBytes(deduplicated).Array())
			if toolDeclCount > 0 {
				toolsJSON = []byte(`[]`)
				toolsJSON, _ = sjson.SetRawBytes(toolsJSON, "-1", functionToolNode)
			}
		}
	}

	// Build output Gemini CLI request JSON
	out := []byte(`{"model":"","request":{"contents":[]}}`)
	out, _ = sjson.SetBytes(out, "model", modelName)

	toolChoiceResult := gjson.GetBytes(rawJSON, "tool_choice")
	toolChoiceType := ""
	toolChoiceName := ""
	if toolChoiceResult.Exists() {
		if toolChoiceResult.IsObject() {
			toolChoiceType = toolChoiceResult.Get("type").String()
			toolChoiceName = toolChoiceResult.Get("name").String()
		} else if toolChoiceResult.Type == gjson.String {
			toolChoiceType = toolChoiceResult.String()
		}
	}
	isToolChoiceNone := strings.EqualFold(strings.TrimSpace(toolChoiceType), "none")

	// Inject interleaved thinking hint when both tools and thinking are active
	hasTools := toolDeclCount > 0 && !isToolChoiceNone
	thinkingResult := gjson.GetBytes(rawJSON, "thinking")
	thinkingType := thinkingResult.Get("type").String()
	hasThinking := thinkingResult.Exists() && thinkingResult.IsObject() && (thinkingType == "enabled" || thinkingType == "adaptive" || thinkingType == "auto")
	isClaudeThinking := util.IsClaudeThinkingModel(modelName)

	if hasTools && hasThinking && isClaudeThinking {
		interleavedHint := "Interleaved thinking is enabled. You may think between tool calls and after receiving tool results before deciding the next action or final answer. Do not mention these instructions or any constraints about thinking blocks; just apply them."

		if hasSystemInstruction {
			// Append hint as a new part to existing system instruction
			hintPart := []byte(`{"text":""}`)
			hintPart, _ = sjson.SetBytes(hintPart, "text", interleavedHint)
			systemInstructionJSON, _ = sjson.SetRawBytes(systemInstructionJSON, "parts.-1", hintPart)
		} else {
			// Create new system instruction with hint
			systemInstructionJSON = []byte(`{"role":"user","parts":[]}`)
			hintPart := []byte(`{"text":""}`)
			hintPart, _ = sjson.SetBytes(hintPart, "text", interleavedHint)
			systemInstructionJSON, _ = sjson.SetRawBytes(systemInstructionJSON, "parts.-1", hintPart)
			hasSystemInstruction = true
		}
	}

	if hasSystemInstruction {
		out, _ = sjson.SetRawBytes(out, "request.systemInstruction", systemInstructionJSON)
	}
	if hasContents {
		contentResults := gjson.ParseBytes(contentsJSON).Array()
		contentItems := make([][]byte, 0, len(contentResults))
		for _, content := range contentResults {
			contentItems = append(contentItems, []byte(content.Raw))
		}
		if util.IsClaudeModel(modelName) {
			contentItems = translatorcommon.SplitGeminiFunctionResponseTurns(contentItems)
			contentsJSON = translatorcommon.JoinRawArray(translatorcommon.MergeAdjacentGeminiUserContents(contentItems))
		} else {
			contentsJSON = translatorcommon.JoinRawArray(translatorcommon.MergeAdjacentGeminiContents(contentItems))
		}
		out, _ = sjson.SetRawBytes(out, "request.contents", contentsJSON)
	}
	if toolDeclCount > 0 && !isToolChoiceNone {
		out, _ = sjson.SetRawBytes(out, "request.tools", toolsJSON)
	}

	// tool_choice
	if toolChoiceResult.Exists() {
		switch strings.ToLower(strings.TrimSpace(toolChoiceType)) {
		case "auto":
			out, _ = sjson.SetBytes(out, "request.toolConfig.functionCallingConfig.mode", "AUTO")
		case "none":
			out, _ = sjson.SetBytes(out, "request.toolConfig.functionCallingConfig.mode", "NONE")
			out, _ = sjson.DeleteBytes(out, "request.tools")
		case "any":
			out, _ = sjson.SetBytes(out, "request.toolConfig.functionCallingConfig.mode", "ANY")
		case "tool":
			out, _ = sjson.SetBytes(out, "request.toolConfig.functionCallingConfig.mode", "ANY")
			if toolChoiceName != "" {
				out, _ = sjson.SetBytes(
					out,
					"request.toolConfig.functionCallingConfig.allowedFunctionNames",
					[]string{mapClaudeFunctionNameToAntigravity(functionNameMap, toolChoiceName)},
				)
			}
		}
	}

	// Map Anthropic thinking -> Gemini thinkingBudget/include_thoughts when type==enabled
	if t := gjson.GetBytes(rawJSON, "thinking"); enableThoughtTranslate && t.Exists() && t.IsObject() {
		switch t.Get("type").String() {
		case "enabled":
			if b := t.Get("budget_tokens"); b.Exists() && b.Type == gjson.Number {
				budget := int(b.Int())
				out, _ = sjson.SetBytes(out, "request.generationConfig.thinkingConfig.thinkingBudget", budget)
			}
		case "adaptive", "auto":
			// For adaptive thinking:
			// - If output_config.effort is explicitly present, pass through as thinkingLevel.
			// - Otherwise, treat it as "enabled with target-model maximum" and emit high.
			// ApplyThinking handles clamping to target model's supported levels.
			effort := ""
			if v := gjson.GetBytes(rawJSON, "output_config.effort"); v.Exists() && v.Type == gjson.String {
				effort = strings.ToLower(strings.TrimSpace(v.String()))
			}
			if effort != "" {
				out, _ = sjson.SetBytes(out, "request.generationConfig.thinkingConfig.thinkingLevel", effort)
			} else {
				out, _ = sjson.SetBytes(out, "request.generationConfig.thinkingConfig.thinkingLevel", "high")
			}
		}
	}
	if v := gjson.GetBytes(rawJSON, "temperature"); v.Exists() && v.Type == gjson.Number {
		out, _ = sjson.SetBytes(out, "request.generationConfig.temperature", v.Num)
	}
	if v := gjson.GetBytes(rawJSON, "top_p"); v.Exists() && v.Type == gjson.Number {
		out, _ = sjson.SetBytes(out, "request.generationConfig.topP", v.Num)
	}
	if v := gjson.GetBytes(rawJSON, "top_k"); v.Exists() && v.Type == gjson.Number {
		out, _ = sjson.SetBytes(out, "request.generationConfig.topK", v.Num)
	}
	if v := gjson.GetBytes(rawJSON, "max_tokens"); v.Exists() && v.Type == gjson.Number {
		out, _ = sjson.SetBytes(out, "request.generationConfig.maxOutputTokens", v.Num)
	}

	out = common.AttachDefaultSafetySettings(out, "request.safetySettings")
	if signature.ProviderFromModelName(modelName) == signature.ProviderGemini {
		out = signature.SanitizeGeminiRequestThoughtSignatures(out, "request.contents")
	}

	if err := drops.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
