// Package chat_completions provides request translation functionality for OpenAI to Gemini CLI API compatibility.
// It converts OpenAI Chat Completions requests into Gemini CLI compatible JSON using gjson/sjson only.
package chat_completions

import (
	"fmt"
	"strings"

	sigcompat "github.com/Pyrokine/CLIProxyAPI/v6/internal/signature"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	antigravitygemini "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/antigravity/gemini"
	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/gemini/common"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const geminiCLIFunctionThoughtSignature = "skip_thought_signature_validator"

func mapOpenAIFunctionNameToAntigravity(functionNameMap map[string]string, name string) string {
	return translatorcommon.AntigravityToolNameToUpstream(
		util.MapSanitizedFunctionName(functionNameMap, name),
	)
}

// ConvertOpenAIRequestToAntigravity converts an OpenAI Chat Completions request (raw JSON)
// into a complete Gemini CLI request JSON. All JSON construction uses sjson and lookups use gjson.
//
// Parameters:
//   - modelName: The name of the model to use for the request
//   - rawJSON: The raw JSON request data from the OpenAI API
//   - stream: A boolean indicating if the request is for a streaming response (unused in current implementation)
//
// Returns:
//   - []byte: The transformed request data in Gemini CLI API format
func ConvertOpenAIRequestToAntigravity(modelName string, inputRawJSON []byte, stream bool) []byte {
	out, _ := convertOpenAIRequestToAntigravityChecked(modelName, inputRawJSON, stream)
	return out
}

func convertOpenAIRequestToAntigravityChecked(modelName string, inputRawJSON []byte, _ bool) ([]byte, error) {
	var drops translatorcommon.UserTurnDrops
	rawJSON := inputRawJSON
	functionNameMap := util.SanitizedFunctionNameMap(rawJSON)
	// Base envelope (no default thinkingConfig)
	out := []byte(`{"project":"","request":{"contents":[]},"model":"gemini-2.5-pro"}`)

	// Model
	out, _ = sjson.SetBytes(out, "model", modelName)

	// Let user-provided generationConfig pass through
	if genConfig := gjson.GetBytes(rawJSON, "generationConfig"); genConfig.Exists() {
		out, _ = sjson.SetRawBytes(out, "request.generationConfig", []byte(genConfig.Raw))
	}

	// Apply thinking configuration: convert OpenAI reasoning_effort to Gemini CLI thinkingConfig.
	// Inline translation-only mapping; capability checks happen later in ApplyThinking.
	re := gjson.GetBytes(rawJSON, "reasoning_effort")
	if re.Exists() {
		effort := strings.ToLower(strings.TrimSpace(re.String()))
		if effort != "" {
			thinkingPath := "request.generationConfig.thinkingConfig"
			if effort == "auto" {
				out, _ = sjson.SetBytes(out, thinkingPath+".thinkingBudget", -1)
			} else {
				out, _ = sjson.SetBytes(out, thinkingPath+".thinkingLevel", effort)
			}
		}
	}

	// Temperature/top_p/top_k/max_tokens/max_completion_tokens
	if tr := gjson.GetBytes(rawJSON, "temperature"); tr.Exists() && tr.Type == gjson.Number {
		out, _ = sjson.SetBytes(out, "request.generationConfig.temperature", tr.Num)
	}
	if tpr := gjson.GetBytes(rawJSON, "top_p"); tpr.Exists() && tpr.Type == gjson.Number {
		out, _ = sjson.SetBytes(out, "request.generationConfig.topP", tpr.Num)
	}
	if tkr := gjson.GetBytes(rawJSON, "top_k"); tkr.Exists() && tkr.Type == gjson.Number {
		out, _ = sjson.SetBytes(out, "request.generationConfig.topK", tkr.Num)
	}
	if maxTok := gjson.GetBytes(rawJSON, "max_tokens"); maxTok.Exists() && maxTok.Type == gjson.Number {
		out, _ = sjson.SetBytes(out, "request.generationConfig.maxOutputTokens", maxTok.Num)
	} else if maxCompletionTokens := gjson.GetBytes(rawJSON, "max_completion_tokens"); maxCompletionTokens.Exists() && maxCompletionTokens.Type == gjson.Number {
		out, _ = sjson.SetBytes(out, "request.generationConfig.maxOutputTokens", maxCompletionTokens.Num)
	}
	out = applyOpenAIResponseFormatToAntigravity(out, rawJSON)

	// Candidate count (OpenAI 'n' parameter)
	if n := gjson.GetBytes(rawJSON, "n"); n.Exists() && n.Type == gjson.Number {
		if val := n.Int(); val > 1 {
			out, _ = sjson.SetBytes(out, "request.generationConfig.candidateCount", val)
		}
	}

	// Map OpenAI modalities -> Gemini CLI request.generationConfig.responseModalities
	// e.g. "modalities": ["image", "text"] -> ["IMAGE", "TEXT"]
	if mods := gjson.GetBytes(rawJSON, "modalities"); mods.Exists() && mods.IsArray() {
		var responseMods []string
		for _, m := range mods.Array() {
			switch strings.ToLower(m.String()) {
			case "text":
				responseMods = append(responseMods, "TEXT")
			case "image":
				responseMods = append(responseMods, "IMAGE")
			}
		}
		if len(responseMods) > 0 {
			out, _ = sjson.SetBytes(out, "request.generationConfig.responseModalities", responseMods)
		}
	}

	// OpenRouter-style image_config support
	// If the input uses top-level image_config.aspect_ratio, map it into request.generationConfig.imageConfig.aspectRatio.
	if imgCfg := gjson.GetBytes(rawJSON, "image_config"); imgCfg.Exists() && imgCfg.IsObject() {
		if ar := imgCfg.Get("aspect_ratio"); ar.Exists() && ar.Type == gjson.String {
			out, _ = sjson.SetBytes(out, "request.generationConfig.imageConfig.aspectRatio", ar.Str)
		}
		if size := imgCfg.Get("image_size"); size.Exists() && size.Type == gjson.String {
			out, _ = sjson.SetBytes(out, "request.generationConfig.imageConfig.imageSize", size.Str)
		}
	}

	// messages -> systemInstruction + contents
	messages := gjson.GetBytes(rawJSON, "messages")
	if messages.IsArray() {
		arr := messages.Array()
		systemPartIndex := 0
		hasEncounteredConversation := false
		for i := range arr {
			m := arr[i]
			role := m.Get("role").String()
			content := m.Get("content")
			if role == "tool" {
				hasEncounteredConversation = true
			}

			isSystemMessage := role == "system" || role == "developer"
			if isSystemMessage && len(arr) > 1 && !hasEncounteredConversation {
				// Leading system/developer messages become the global system instruction.
				if content.Type == gjson.String {
					out, _ = sjson.SetBytes(out, "request.systemInstruction.role", "user")
					out, _ = sjson.SetBytes(
						out, fmt.Sprintf("request.systemInstruction.parts.%d.text", systemPartIndex), content.String(),
					)
					systemPartIndex++
				} else if content.IsObject() && content.Get("type").String() == "text" {
					out, _ = sjson.SetBytes(out, "request.systemInstruction.role", "user")
					out, _ = sjson.SetBytes(
						out, fmt.Sprintf("request.systemInstruction.parts.%d.text", systemPartIndex),
						content.Get("text").String(),
					)
					systemPartIndex++
				} else if content.IsArray() {
					contents := content.Array()
					if len(contents) > 0 {
						out, _ = sjson.SetBytes(out, "request.systemInstruction.role", "user")
						for j := range contents {
							out, _ = sjson.SetBytes(
								out, fmt.Sprintf("request.systemInstruction.parts.%d.text", systemPartIndex),
								contents[j].Get("text").String(),
							)
							systemPartIndex++
						}
					}
				}
			} else if role == "user" || isSystemMessage {
				isDemotedSystem := isSystemMessage && hasEncounteredConversation
				hasEncounteredConversation = true
				// Build one user content node without changing a mid-session
				// system/developer message into a global instruction.
				node := []byte(`{"role":"user","parts":[]}`)
				if isDemotedSystem {
					reminderContent := content
					if content.IsObject() && content.Get("type").String() == "text" {
						reminderContent = content.Get("text")
					}
					if reminder, ok := translatorcommon.ClaudeMessageSystemReminderText(reminderContent); ok {
						node, _ = sjson.SetBytes(node, "parts.0.text", reminder)
						out, _ = sjson.SetRawBytes(out, "request.contents.-1", node)
					}
					continue
				}
				if content.Type == gjson.String {
					node, _ = sjson.SetBytes(node, "parts.0.text", content.String())
				} else if content.IsObject() && content.Get("type").String() == "text" {
					node, _ = sjson.SetBytes(node, "parts.0.text", content.Get("text").String())
				} else if content.IsArray() {
					items := content.Array()
					p := 0
					for _, item := range items {
						switch item.Get("type").String() {
						case "text":
							text := item.Get("text").String()
							if text != "" {
								node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".text", text)
								p++
							}
						case "image_url":
							if part, ok := antigravityOpenAIDataURLPart(
								item.Get("image_url.url").String(), "image/",
							); ok {
								part, _ = sjson.SetBytes(part, "thoughtSignature", geminiCLIFunctionThoughtSignature)
								node, _ = sjson.SetRawBytes(node, "parts."+itoa(p), part)
								p++
							} else if role == "user" {
								drops.Drop("image_url")
							}
						case "video_url":
							if part, ok := antigravityOpenAIDataURLPart(
								item.Get("video_url.url").String(), "video/",
							); ok {
								node, _ = sjson.SetRawBytes(node, "parts."+itoa(p), part)
								p++
							} else if role == "user" {
								drops.Drop("video_url")
							}
						case "file":
							filename := item.Get("file.filename").String()
							fileData := item.Get("file.file_data").String()
							if mimeType, data, ok := translatorcommon.NormalizeOpenAIFileData(
								filename, "", fileData,
							); ok {
								node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".inlineData.mimeType", mimeType)
								node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".inlineData.data", data)
								p++
							} else {
								if role == "user" {
									drops.Drop("file")
								}
								log.Warn("Invalid file data or unknown file name extension in user message, skip")
							}
						case "input_audio":
							audioData := item.Get("input_audio.data").String()
							audioFormat := item.Get("input_audio.format").String()
							if audioData != "" {
								audioMimeMap := map[string]string{
									"mp3":       "audio/mpeg",
									"wav":       "audio/wav",
									"ogg":       "audio/ogg",
									"flac":      "audio/flac",
									"aac":       "audio/aac",
									"webm":      "audio/webm",
									"pcm16":     "audio/pcm",
									"g711_ulaw": "audio/basic",
									"g711_alaw": "audio/basic",
								}
								mimeType := "audio/wav"
								if audioFormat != "" {
									if mapped, ok := audioMimeMap[audioFormat]; ok {
										mimeType = mapped
									} else {
										mimeType = "audio/" + audioFormat
									}
								}
								node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".inlineData.mime_type", mimeType)
								node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".inlineData.data", audioData)
								p++
							} else if role == "user" {
								drops.Drop("input_audio")
							}
						case "input_file", "audio", "video":
							if role == "user" {
								drops.Drop(item.Get("type").String())
							}
						}
					}
				}
				if role == "user" {
					sendable := 0
					for _, part := range gjson.GetBytes(node, "parts").Array() {
						if translatorcommon.GeminiPartIsSendable([]byte(part.Raw)) {
							sendable++
						}
					}
					drops.EndTurn(sendable)
				}
				if len(gjson.GetBytes(node, "parts").Array()) > 0 {
					out, _ = sjson.SetRawBytes(out, "request.contents.-1", node)
				}
			} else if role == "assistant" {
				hasEncounteredConversation = true
				node := []byte(`{"role":"model","parts":[]}`)
				p := 0
				if reasoningContent := m.Get("reasoning_content"); reasoningContent.Type == gjson.String && reasoningContent.String() != "" {
					node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".text", reasoningContent.String())
					node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".thought", true)
					node, _ = sjson.SetBytes(
						node, "parts."+itoa(p)+".thoughtSignature", geminiCLIFunctionThoughtSignature,
					)
					p++
				}
				if content.Type == gjson.String && content.String() != "" {
					node, _ = sjson.SetBytes(node, "parts.-1.text", content.String())
					p++
				} else if content.IsArray() {
					// Assistant multimodal content (e.g. text + image) -> single model content with parts
					for _, item := range content.Array() {
						switch item.Get("type").String() {
						case "text":
							text := item.Get("text").String()
							if text != "" {
								node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".text", text)
								p++
							}
						case "image_url":
							// Preserve only inline image data; history translation never downloads URLs.
							if part, ok := antigravityOpenAIDataURLPart(
								item.Get("image_url.url").String(), "image/",
							); ok {
								part, _ = sjson.SetBytes(
									part, "thoughtSignature", geminiCLIFunctionThoughtSignature,
								)
								node, _ = sjson.SetRawBytes(node, "parts."+itoa(p), part)
								p++
							}
						}
					}
				}

				// Tool calls -> single model content with functionCall parts
				tcs := m.Get("tool_calls")
				if tcs.IsArray() {
					type assistantToolCall struct{ rawID, id, name string }
					toolCalls := make([]assistantToolCall, 0, len(tcs.Array()))
					usedIDs := make(map[string]bool, len(tcs.Array()))
					for _, tc := range tcs.Array() {
						if tc.Get("type").String() != "function" {
							continue
						}
						rawID := tc.Get("id").String()
						fid := rawID
						if sigcompat.ProviderFromModelName(modelName) == sigcompat.ProviderClaude {
							base := util.SanitizeClaudeToolID(rawID)
							fid = base
							for suffix := 1; usedIDs[fid]; suffix++ {
								fid = fmt.Sprintf("%s_%d", base, suffix)
							}
							usedIDs[fid] = true
						}
						functionName := mapOpenAIFunctionNameToAntigravity(
							functionNameMap,
							tc.Get("function.name").String(),
						)
						if functionName == "" {
							continue
						}
						functionArguments := tc.Get("function.arguments").String()
						node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".functionCall.id", fid)
						node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".functionCall.name", functionName)
						if gjson.Valid(functionArguments) {
							node, _ = sjson.SetRawBytes(node, "parts."+itoa(p)+".functionCall.args", []byte(functionArguments))
						} else {
							node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".functionCall.args.params", []byte(functionArguments))
						}
						node, _ = sjson.SetBytes(
							node, "parts."+itoa(p)+".thoughtSignature", geminiCLIFunctionThoughtSignature,
						)
						p++
						if fid != "" {
							toolCalls = append(toolCalls, assistantToolCall{rawID: rawID, id: fid, name: functionName})
						}
					}
					if p > 0 {
						out, _ = sjson.SetRawBytes(out, "request.contents.-1", node)
					}

					// Keep tool results associated with the assistant turn that issued the calls.
					turnToolResponses := make(map[string]gjson.Result, len(toolCalls))
					for j := i + 1; j < len(arr) && arr[j].Get("role").String() != "assistant"; j++ {
						if arr[j].Get("role").String() == "tool" {
							if callID := arr[j].Get("tool_call_id").String(); callID != "" {
								turnToolResponses[callID] = arr[j].Get("content")
							}
						}
					}
					toolNode := []byte(`{"role":"user","parts":[]}`)
					pp := 0
					for _, call := range toolCalls {
						toolNode, _ = sjson.SetBytes(toolNode, "parts."+itoa(pp)+".functionResponse.id", call.id)
						toolNode, _ = sjson.SetBytes(toolNode, "parts."+itoa(pp)+".functionResponse.name", call.name)
						response, exists := turnToolResponses[call.rawID]
						toolNode = setAntigravityOpenAIToolResponse(
							toolNode,
							"parts."+itoa(pp)+".functionResponse.response.result",
							response,
							exists,
						)
						pp++
					}
					if pp > 0 {
						out, _ = sjson.SetRawBytes(out, "request.contents.-1", toolNode)
					}
				} else if p > 0 {
					out, _ = sjson.SetRawBytes(out, "request.contents.-1", node)
				}
			}
		}
	}

	// tools -> request.tools[].functionDeclarations + request.tools[].googleSearch/codeExecution/urlContext passthrough
	tools := gjson.GetBytes(rawJSON, "tools")
	if tools.IsArray() && len(tools.Array()) > 0 {
		functionToolNode := []byte(`{}`)
		hasFunction := false
		googleSearchNodes := make([][]byte, 0)
		codeExecutionNodes := make([][]byte, 0)
		urlContextNodes := make([][]byte, 0)
		for _, t := range tools.Array() {
			if t.Get("type").String() == "function" {
				fn := t.Get("function")
				if fn.Exists() && fn.IsObject() {
					fnRaw := fn.Raw
					if fn.Get("parameters").Exists() {
						renamed, errRename := util.RenameKey(fnRaw, "parameters", "parametersJsonSchema")
						if errRename != nil {
							log.Warnf(
								"Failed to rename parameters for tool '%s': %v", fn.Get("name").String(), errRename,
							)
							var errSet error
							fnRawBytes, errSet := sjson.SetBytes([]byte(fnRaw), "parametersJsonSchema.type", "object")
							if errSet != nil {
								log.Warnf(
									"Failed to set default schema type for tool '%s': %v", fn.Get("name").String(),
									errSet,
								)
								continue
							}
							fnRaw = string(fnRawBytes)
							fnRawBytes, errSet = sjson.SetRawBytes(
								[]byte(fnRaw), "parametersJsonSchema.properties", []byte(`{}`),
							)
							if errSet != nil {
								log.Warnf(
									"Failed to set default schema properties for tool '%s': %v",
									fn.Get("name").String(), errSet,
								)
								continue
							}
							fnRaw = string(fnRawBytes)
						} else {
							fnRaw = renamed
						}
					} else {
						var errSet error
						fnRawBytes, errSet := sjson.SetBytes([]byte(fnRaw), "parametersJsonSchema.type", "object")
						if errSet != nil {
							log.Warnf(
								"Failed to set default schema type for tool '%s': %v", fn.Get("name").String(), errSet,
							)
							continue
						}
						fnRaw = string(fnRawBytes)
						fnRawBytes, errSet = sjson.SetRawBytes(
							[]byte(fnRaw), "parametersJsonSchema.properties", []byte(`{}`),
						)
						if errSet != nil {
							log.Warnf(
								"Failed to set default schema properties for tool '%s': %v", fn.Get("name").String(),
								errSet,
							)
							continue
						}
						fnRaw = string(fnRawBytes)
					}
					fnRawBytes := []byte(fnRaw)
					nameResult := fn.Get("name")
					originalName := nameResult.String()
					mappedName := mapOpenAIFunctionNameToAntigravity(functionNameMap, originalName)
					if nameResult.Type != gjson.String || mappedName != originalName {
						fnRawBytes, _ = sjson.SetBytes(fnRawBytes, "name", mappedName)
					}
					if gjson.GetBytes(fnRawBytes, "strict").Exists() {
						fnRawBytes, _ = sjson.DeleteBytes(fnRawBytes, "strict")
					}
					fnRaw = string(fnRawBytes)
					if !hasFunction {
						functionToolNode, _ = sjson.SetRawBytes(functionToolNode, "functionDeclarations", []byte("[]"))
					}
					tmp, errSet := sjson.SetRawBytes(functionToolNode, "functionDeclarations.-1", []byte(fnRaw))
					if errSet != nil {
						log.Warnf("Failed to append tool declaration for '%s': %v", fn.Get("name").String(), errSet)
						continue
					}
					functionToolNode = tmp
					hasFunction = true
				}
			}
			if gs := t.Get("google_search"); gs.Exists() {
				googleToolNode := []byte(`{}`)
				var errSet error
				googleToolNode, errSet = sjson.SetRawBytes(googleToolNode, "googleSearch", []byte(gs.Raw))
				if errSet != nil {
					log.Warnf("Failed to set googleSearch tool: %v", errSet)
					continue
				}
				googleSearchNodes = append(googleSearchNodes, googleToolNode)
			}
			if ce := t.Get("code_execution"); ce.Exists() {
				codeToolNode := []byte(`{}`)
				var errSet error
				codeToolNode, errSet = sjson.SetRawBytes(codeToolNode, "codeExecution", []byte(ce.Raw))
				if errSet != nil {
					log.Warnf("Failed to set codeExecution tool: %v", errSet)
					continue
				}
				codeExecutionNodes = append(codeExecutionNodes, codeToolNode)
			}
			if uc := t.Get("url_context"); uc.Exists() {
				urlToolNode := []byte(`{}`)
				var errSet error
				urlToolNode, errSet = sjson.SetRawBytes(urlToolNode, "urlContext", []byte(uc.Raw))
				if errSet != nil {
					log.Warnf("Failed to set urlContext tool: %v", errSet)
					continue
				}
				urlContextNodes = append(urlContextNodes, urlToolNode)
			}
		}
		if hasFunction {
			declarations := gjson.GetBytes(functionToolNode, "functionDeclarations")
			deduplicated := util.DeduplicateFunctionDeclarations([]byte(declarations.Raw))
			functionToolNode, _ = sjson.SetRawBytes(
				functionToolNode,
				"functionDeclarations",
				deduplicated,
			)
			hasFunction = len(gjson.ParseBytes(deduplicated).Array()) > 0
		}
		if hasFunction || len(googleSearchNodes) > 0 || len(codeExecutionNodes) > 0 || len(urlContextNodes) > 0 {
			toolsNode := []byte("[]")
			if hasFunction {
				toolsNode, _ = sjson.SetRawBytes(toolsNode, "-1", functionToolNode)
			}
			for _, googleNode := range googleSearchNodes {
				toolsNode, _ = sjson.SetRawBytes(toolsNode, "-1", googleNode)
			}
			for _, codeNode := range codeExecutionNodes {
				toolsNode, _ = sjson.SetRawBytes(toolsNode, "-1", codeNode)
			}
			for _, urlNode := range urlContextNodes {
				toolsNode, _ = sjson.SetRawBytes(toolsNode, "-1", urlNode)
			}
			out, _ = sjson.SetRawBytes(out, "request.tools", toolsNode)
		}
	}

	out = applyOpenAIToolChoiceToAntigravity(out, rawJSON, functionNameMap)
	out = antigravitygemini.SanitizeAntigravityClaudeGeminiRequestSignatures(modelName, out)
	out = common.AttachDefaultSafetySettings(out, "request.safetySettings")
	if err := drops.Err(); err != nil {
		return nil, err
	}
	return applyOpenAIThinkingCompatibilityToAntigravity(out, rawJSON), nil
}

func antigravityOpenAIDataURLPart(dataURL, requiredMIMEPrefix string) ([]byte, bool) {
	mimeType, data, ok := translatorcommon.NormalizeOpenAIFileData("", "", dataURL)
	if !ok || !strings.HasPrefix(strings.ToLower(strings.TrimSpace(mimeType)), requiredMIMEPrefix) {
		return nil, false
	}
	part := []byte(`{"inlineData":{"mimeType":"","data":""}}`)
	part, _ = sjson.SetBytes(part, "inlineData.mimeType", mimeType)
	part, _ = sjson.SetBytes(part, "inlineData.data", data)
	return part, true
}

func setAntigravityOpenAIToolResponse(
	payload []byte,
	path string,
	response gjson.Result,
	exists bool,
) []byte {
	if !exists || !response.Exists() {
		payload, _ = sjson.SetRawBytes(payload, path, []byte(`{}`))
		return payload
	}
	if response.Type == gjson.String {
		payload, _ = sjson.SetBytes(payload, path, response.String())
		return payload
	}
	if response.Raw != "" {
		payload, _ = sjson.SetRawBytes(payload, path, []byte(response.Raw))
	}
	return payload
}

func applyOpenAIResponseFormatToAntigravity(out, rawJSON []byte) []byte {
	responseFormat := gjson.GetBytes(rawJSON, "response_format")
	if !responseFormat.IsObject() {
		return out
	}
	responseFormatType := strings.ToLower(strings.TrimSpace(responseFormat.Get("type").String()))
	if responseFormatType != "json_object" && responseFormatType != "json_schema" {
		return out
	}

	for _, schemaKey := range []string{
		"responseSchema",
		"responseJsonSchema",
		"response_schema",
		"response_json_schema",
	} {
		out, _ = sjson.DeleteBytes(out, "request.generationConfig."+schemaKey)
	}
	out, _ = sjson.SetBytes(out, "request.generationConfig.responseMimeType", "application/json")
	if responseFormatType == "json_object" {
		return out
	}

	schema := responseFormat.Get("json_schema.schema")
	if !schema.IsObject() {
		log.Warn("OpenAI response_format json_schema requires an object schema")
		return out
	}
	cleanedSchema := util.CleanJSONSchemaForAntigravityResponse(schema.Raw)
	out, _ = sjson.SetRawBytes(
		out,
		"request.generationConfig.responseSchema",
		[]byte(cleanedSchema),
	)
	return out
}

func applyOpenAIThinkingCompatibilityToAntigravity(out, rawJSON []byte) []byte {
	out = normalizeAntigravityOpenAIThinkingConfig(out)
	config := thinking.ExtractSummaryConfig(rawJSON, "openai")
	return thinking.ApplySummaryConfig(out, "antigravity", config)
}

func normalizeAntigravityOpenAIThinkingConfig(out []byte) []byte {
	for _, prefix := range []string{
		"request.generationConfig.thinking_config",
		"request.generationConfig.thinkingConfig",
	} {
		for _, field := range []string{"includeThoughts", "include_thoughts"} {
			sourcePath := prefix + "." + field
			value := gjson.GetBytes(out, sourcePath)
			if !value.Exists() {
				continue
			}
			out = setAntigravityOpenAIBoolResultIfValid(
				out,
				"request.generationConfig.thinkingConfig.includeThoughts",
				value,
			)
		}
		for _, field := range []string{"thinkingLevel", "thinking_level"} {
			if value := gjson.GetBytes(out, prefix+"."+field); value.Exists() {
				out = setAntigravityOpenAIRawIfDifferent(
					out,
					"request.generationConfig.thinkingConfig.thinkingLevel",
					value,
				)
			}
		}
		for _, field := range []string{"thinkingBudget", "thinking_budget"} {
			if value := gjson.GetBytes(out, prefix+"."+field); value.Exists() {
				out = setAntigravityOpenAIRawIfDifferent(
					out,
					"request.generationConfig.thinkingConfig.thinkingBudget",
					value,
				)
			}
		}
	}

	for _, path := range []string{
		"request.generationConfig.includeThoughts",
		"request.generationConfig.include_thoughts",
	} {
		if value := gjson.GetBytes(out, path); value.Exists() {
			out = setAntigravityOpenAIBoolResultIfValid(
				out,
				"request.generationConfig.thinkingConfig.includeThoughts",
				value,
			)
		}
	}

	for _, path := range []string{
		"request.generationConfig.thinking_config",
		"request.generationConfig.thinkingConfig.include_thoughts",
		"request.generationConfig.thinkingConfig.thinking_level",
		"request.generationConfig.thinkingConfig.thinking_budget",
		"request.generationConfig.includeThoughts",
		"request.generationConfig.include_thoughts",
	} {
		if gjson.GetBytes(out, path).Exists() {
			out, _ = sjson.DeleteBytes(out, path)
		}
	}
	if include := gjson.GetBytes(out, "request.generationConfig.thinkingConfig.includeThoughts"); include.Exists() && include.Type != gjson.True && include.Type != gjson.False {
		out, _ = sjson.DeleteBytes(out, "request.generationConfig.thinkingConfig.includeThoughts")
	}
	return out
}

func setAntigravityOpenAIBoolResultIfValid(out []byte, path string, value gjson.Result) []byte {
	switch value.Type {
	case gjson.True:
		return setAntigravityOpenAIBoolIfDifferent(out, path, true)
	case gjson.False:
		return setAntigravityOpenAIBoolIfDifferent(out, path, false)
	default:
		return out
	}
}

func setAntigravityOpenAIBoolIfDifferent(out []byte, path string, value bool) []byte {
	current := gjson.GetBytes(out, path)
	if value && current.Type == gjson.True || !value && current.Type == gjson.False {
		return out
	}
	updated, errSet := sjson.SetBytes(out, path, value)
	if errSet != nil {
		return out
	}
	return updated
}

func setAntigravityOpenAIRawIfDifferent(out []byte, path string, value gjson.Result) []byte {
	current := gjson.GetBytes(out, path)
	if current.Exists() && current.Raw == value.Raw {
		return out
	}
	updated, errSet := sjson.SetRawBytes(out, path, []byte(value.Raw))
	if errSet != nil {
		return out
	}
	return updated
}

func applyOpenAIToolChoiceToAntigravity(
	out []byte,
	rawJSON []byte,
	functionNameMap map[string]string,
) []byte {
	toolChoice := gjson.GetBytes(rawJSON, "tool_choice")
	if !toolChoice.Exists() {
		return out
	}

	mode := ""
	allowedName := ""
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
		switch strings.ToLower(strings.TrimSpace(toolChoice.Get("type").String())) {
		case "none":
			mode = "NONE"
		case "function":
			mode = "ANY"
			allowedName = toolChoice.Get("function.name").String()
		}
	}
	if mode == "" {
		return out
	}

	out, _ = sjson.SetBytes(out, "request.toolConfig.functionCallingConfig.mode", mode)
	if mode == "NONE" {
		out, _ = sjson.DeleteBytes(out, "request.tools")
	}
	if strings.TrimSpace(allowedName) != "" {
		mappedName := mapOpenAIFunctionNameToAntigravity(functionNameMap, allowedName)
		out, _ = sjson.SetBytes(
			out,
			"request.toolConfig.functionCallingConfig.allowedFunctionNames",
			[]string{mappedName},
		)
	}
	return out
}

// itoa converts int to string without strconv import for few usages.
func itoa(i int) string { return fmt.Sprintf("%d", i) }
