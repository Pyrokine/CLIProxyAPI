// Package chat_completions provides request translation functionality for OpenAI to Gemini API compatibility.
// It converts OpenAI Chat Completions requests into Gemini compatible JSON using gjson/sjson only.
package chat_completions

import (
	"encoding/base64"
	"fmt"
	"mime"
	"strings"

	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/gemini/common"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const geminiFunctionThoughtSignature = "skip_thought_signature_validator"

// convertOpenAIRequestToGemini converts an OpenAI Chat Completions request (raw JSON)
// into a complete Gemini request JSON. All JSON construction uses sjson and lookups use gjson.
//
// Parameters:
//   - modelName: The name of the model to use for the request
//   - rawJSON: The raw JSON request data from the OpenAI API
//   - stream: A boolean indicating if the request is for a streaming response (unused in current implementation)
//
// Returns:
//   - []byte: The transformed request data in Gemini API format
func convertOpenAIRequestToGemini(modelName string, inputRawJSON []byte, stream bool) []byte {
	body, _ := convertOpenAIRequestToGeminiChecked(modelName, inputRawJSON, stream)
	return body
}

func convertOpenAIRequestToGeminiChecked(modelName string, inputRawJSON []byte, _ bool) ([]byte, error) {
	var drops translatorcommon.UserTurnDrops
	rawJSON := inputRawJSON
	// Base envelope (no default thinkingConfig)
	out := []byte(`{"contents":[]}`)

	// Model
	out, _ = sjson.SetBytes(out, "model", modelName)

	// Let user-provided generationConfig pass through
	if genConfig := gjson.GetBytes(rawJSON, "generationConfig"); genConfig.Exists() {
		out, _ = sjson.SetRawBytes(out, "generationConfig", []byte(genConfig.Raw))
	}

	// Apply thinking configuration: convert OpenAI reasoning_effort to Gemini thinkingConfig.
	// Inline translation-only mapping; capability checks happen later in ApplyThinking.
	re := gjson.GetBytes(rawJSON, "reasoning_effort")
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

	// Temperature/top_p/top_k
	if tr := gjson.GetBytes(rawJSON, "temperature"); tr.Exists() && tr.Type == gjson.Number {
		out, _ = sjson.SetBytes(out, "generationConfig.temperature", tr.Num)
	}
	if tpr := gjson.GetBytes(rawJSON, "top_p"); tpr.Exists() && tpr.Type == gjson.Number {
		out, _ = sjson.SetBytes(out, "generationConfig.topP", tpr.Num)
	}
	if tkr := gjson.GetBytes(rawJSON, "top_k"); tkr.Exists() && tkr.Type == gjson.Number {
		out, _ = sjson.SetBytes(out, "generationConfig.topK", tkr.Num)
	}

	// OpenAI max_tokens / max_completion_tokens -> Gemini generationConfig.maxOutputTokens
	if mt := gjson.GetBytes(rawJSON, "max_tokens"); mt.Exists() && mt.Type == gjson.Number {
		out, _ = sjson.SetBytes(out, "generationConfig.maxOutputTokens", mt.Num)
	} else if mct := gjson.GetBytes(rawJSON, "max_completion_tokens"); mct.Exists() && mct.Type == gjson.Number {
		out, _ = sjson.SetBytes(out, "generationConfig.maxOutputTokens", mct.Num)
	}

	// Candidate count (OpenAI 'n' parameter)
	if n := gjson.GetBytes(rawJSON, "n"); n.Exists() && n.Type == gjson.Number {
		if val := n.Int(); val > 1 {
			out, _ = sjson.SetBytes(out, "generationConfig.candidateCount", val)
		}
	}

	out = applyOpenAIResponseFormatToGemini(out, rawJSON)

	// Map OpenAI modalities -> Gemini generationConfig.responseModalities
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
			out, _ = sjson.SetBytes(out, "generationConfig.responseModalities", responseMods)
		}
	}

	// OpenRouter-style image_config support
	// If the input uses top-level image_config.aspect_ratio, map it into generationConfig.imageConfig.aspectRatio.
	if imgCfg := gjson.GetBytes(rawJSON, "image_config"); imgCfg.Exists() && imgCfg.IsObject() {
		if ar := imgCfg.Get("aspect_ratio"); ar.Exists() && ar.Type == gjson.String {
			out, _ = sjson.SetBytes(out, "generationConfig.imageConfig.aspectRatio", ar.Str)
		}
		if size := imgCfg.Get("image_size"); size.Exists() && size.Type == gjson.String {
			out, _ = sjson.SetBytes(out, "generationConfig.imageConfig.imageSize", size.Str)
		}
	}

	// messages -> systemInstruction + contents
	messages := gjson.GetBytes(rawJSON, "messages")
	if messages.IsArray() {
		arr := messages.Array()
		consumedToolMessages := make(map[int]bool)
		hasEncounteredConversation := false
		systemPartIndex := 0
		for i := range arr {
			m := arr[i]
			role := m.Get("role").String()
			content := m.Get("content")
			isDemotedSystem := (role == "system" || role == "developer") && hasEncounteredConversation
			if role != "system" && role != "developer" {
				hasEncounteredConversation = true
			}

			if (role == "system" || role == "developer") && len(arr) > 1 && !isDemotedSystem {
				// system -> systemInstruction as a user message style
				if content.Type == gjson.String {
					out, _ = sjson.SetBytes(out, "systemInstruction.role", "user")
					out, _ = sjson.SetBytes(
						out, fmt.Sprintf("systemInstruction.parts.%d.text", systemPartIndex), content.String(),
					)
					systemPartIndex++
				} else if content.IsObject() && content.Get("type").String() == "text" {
					out, _ = sjson.SetBytes(out, "systemInstruction.role", "user")
					out, _ = sjson.SetBytes(
						out, fmt.Sprintf("systemInstruction.parts.%d.text", systemPartIndex),
						content.Get("text").String(),
					)
					systemPartIndex++
				} else if content.IsArray() {
					contents := content.Array()
					if len(contents) > 0 {
						out, _ = sjson.SetBytes(out, "systemInstruction.role", "user")
						for j := range contents {
							out, _ = sjson.SetBytes(
								out, fmt.Sprintf("systemInstruction.parts.%d.text", systemPartIndex),
								contents[j].Get("text").String(),
							)
							systemPartIndex++
						}
					}
				}
			} else if role == "user" || role == "system" || role == "developer" {
				// Build single user content node to avoid splitting into multiple contents
				node := []byte(`{"role":"user","parts":[]}`)
				if isDemotedSystem {
					reminderContent := content
					if content.IsObject() && content.Get("type").String() == "text" {
						reminderContent = content.Get("text")
					}
					if reminder, ok := translatorcommon.ClaudeMessageSystemReminderText(reminderContent); ok {
						node, _ = sjson.SetBytes(node, "parts.0.text", reminder)
						out, _ = sjson.SetRawBytes(out, "contents.-1", node)
					}
					continue
				}
				if content.Type == gjson.String {
					node, _ = sjson.SetBytes(node, "parts.0.text", content.String())
				} else if content.IsObject() && content.Get("type").String() == "text" {
					node, _ = sjson.SetBytes(node, "parts.0.text", content.Get("text").String())
				} else if content.IsObject() && role == "user" {
					switch content.Get("type").String() {
					case "file", "input_audio", "image_url", "video_url":
						drops.Drop(content.Get("type").String())
					}
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
							before := p
							imageURL := item.Get("image_url.url").String()
							if len(imageURL) > 5 {
								pieces := strings.SplitN(imageURL[5:], ";", 2)
								if len(pieces) == 2 && len(pieces[1]) > 7 {
									mime := pieces[0]
									data := pieces[1][7:]
									node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".inlineData.mime_type", mime)
									node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".inlineData.data", data)
									p++
								}
							}
							if role == "user" && p == before {
								drops.Drop("image_url")
							}
						case "video_url":
							videoURL := item.Get("video_url.url").String()
							videoMIME, videoData, ok := parseOpenAIVideoDataURL(videoURL)
							if ok {
								node, _ = sjson.SetBytes(
									node, "parts."+itoa(p)+".inlineData.mime_type", videoMIME,
								)
								node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".inlineData.data", videoData)
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
								node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".inlineData.mime_type", mimeType)
								node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".inlineData.data", data)
								p++
							} else {
								log.Warn("Invalid file data or unknown file name extension in user message, skip")
								if role == "user" {
									drops.Drop("file")
								}
							}
						case "input_audio":
							data := item.Get("input_audio.data").String()
							if data != "" {
								format := item.Get("input_audio.format").String()
								mimeType := "audio/" + format
								switch format {
								case "", "wav":
									mimeType = "audio/wav"
								case "mp3":
									mimeType = "audio/mpeg"
								case "pcm16":
									mimeType = "audio/pcm"
								case "g711_ulaw", "g711_alaw":
									mimeType = "audio/basic"
								}
								node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".inlineData.mime_type", mimeType)
								node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".inlineData.data", data)
								p++
							} else if role == "user" {
								drops.Drop("input_audio")
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
				out, _ = sjson.SetRawBytes(out, "contents.-1", node)
			} else if role == "assistant" {
				node := []byte(`{"role":"model","parts":[]}`)
				p := 0
				if reasoningContent := m.Get("reasoning_content"); reasoningContent.Type == gjson.String && reasoningContent.String() != "" {
					node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".text", reasoningContent.String())
					node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".thought", true)
					node, _ = sjson.SetBytes(
						node, "parts."+itoa(p)+".thoughtSignature", geminiFunctionThoughtSignature,
					)
					p++
				}
				if content.Type == gjson.String && content.String() != "" {
					// Assistant text -> single model content
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
							// If the assistant returned an inline data URL, preserve it for history fidelity.
							imageURL := item.Get("image_url.url").String()
							if len(imageURL) > 5 { // expect data:...
								pieces := strings.SplitN(imageURL[5:], ";", 2)
								if len(pieces) == 2 && len(pieces[1]) > 7 {
									mime := pieces[0]
									data := pieces[1][7:]
									node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".inlineData.mime_type", mime)
									node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".inlineData.data", data)
									p++
								}
							}
						}
					}
				}

				// Tool calls -> single model content with functionCall parts
				tcs := m.Get("tool_calls")
				if tcs.IsArray() {
					type assistantToolCall struct {
						id   string
						name string
					}
					toolCalls := make([]assistantToolCall, 0, len(tcs.Array()))
					for _, tc := range tcs.Array() {
						if tc.Get("type").String() != "function" {
							continue
						}
						functionName := util.SanitizeFunctionName(tc.Get("function.name").String())
						if functionName == "" {
							continue
						}
						functionArguments := tc.Get("function.arguments").String()
						node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".functionCall.name", functionName)
						node, _ = sjson.SetRawBytes(node, "parts."+itoa(p)+".functionCall.args", []byte(functionArguments))
						node, _ = sjson.SetBytes(
							node, "parts."+itoa(p)+".thoughtSignature", geminiFunctionThoughtSignature,
						)
						p++
						toolCalls = append(toolCalls, assistantToolCall{tc.Get("id").String(), functionName})
					}
					if p > 0 {
						out, _ = sjson.SetRawBytes(out, "contents.-1", node)
					}

					// Only a complete, unambiguous block of immediate tool results can be attached to this turn.
					responseByID := make(map[string]gjson.Result)
					responseIndices := make([]int, 0, len(toolCalls))
					for j := i + 1; j < len(arr) && arr[j].Get("role").String() == "tool"; j++ {
						responseIndices = append(responseIndices, j)
						responseByID[arr[j].Get("tool_call_id").String()] = arr[j].Get("content")
					}
					if len(toolCalls) > 0 && len(responseIndices) == len(toolCalls) {
						toolNode := []byte(`{"role":"user","parts":[]}`)
						seenIDs := make(map[string]bool, len(toolCalls))
						validResponses := len(responseByID) == len(toolCalls)
						for index, call := range toolCalls {
							response, found := responseByID[call.id]
							if call.id == "" || seenIDs[call.id] || !found || !response.Exists() {
								validResponses = false
								break
							}
							seenIDs[call.id] = true
							part := []byte(`{"functionResponse":{"name":"","response":{"result":""}}}`)
							part, _ = sjson.SetBytes(part, "functionResponse.name", call.name)
							part = translatorcommon.SetGeminiFunctionResponseResult(
								part, "functionResponse.response.result", response,
							)
							toolNode, _ = sjson.SetRawBytes(toolNode, "parts."+itoa(index), part)
						}
						if validResponses {
							out, _ = sjson.SetRawBytes(out, "contents.-1", toolNode)
							for _, index := range responseIndices {
								consumedToolMessages[index] = true
							}
						}
					}
				} else if p > 0 {
					out, _ = sjson.SetRawBytes(out, "contents.-1", node)
				}
			} else if role == "tool" && !consumedToolMessages[i] {
				result := content.Raw
				if content.Type == gjson.String {
					result = content.String()
				}
				node := []byte(`{"role":"user","parts":[]}`)
				node, _ = sjson.SetBytes(
					node, "parts.0.text", fmt.Sprintf("Tool result (call_id=%s): %s", m.Get("tool_call_id").String(), result),
				)
				out, _ = sjson.SetRawBytes(out, "contents.-1", node)
			}
		}
		contents := gjson.GetBytes(out, "contents").Array()
		if count := len(contents); count > 0 && contents[count-1].Get("role").String() == "model" {
			out, _ = sjson.DeleteBytes(out, "contents."+itoa(count-1))
		}
	}

	// tools -> tools[].functionDeclarations + tools[].googleSearch/codeExecution/urlContext passthrough
	toolChoice := gjson.GetBytes(rawJSON, "tool_choice")
	isAllowedTools := toolChoice.IsObject() && toolChoice.Get("type").String() == "allowed_tools"
	allowedToolNames := make(map[string]bool)
	allowedMode := "auto"
	if isAllowedTools {
		allowed := toolChoice.Get("allowed_tools.tools")
		if !allowed.IsArray() {
			allowed = toolChoice.Get("tools")
		}
		for _, entry := range allowed.Array() {
			name := strings.TrimSpace(entry.Get("function.name").String())
			if name == "" {
				name = strings.TrimSpace(entry.Get("name").String())
			}
			if name != "" {
				allowedToolNames[name] = true
			}
		}
		allowedMode = strings.ToLower(strings.TrimSpace(toolChoice.Get("allowed_tools.mode").String()))
		if allowedMode == "" {
			allowedMode = strings.ToLower(strings.TrimSpace(toolChoice.Get("mode").String()))
		}
	}
	declaredNames := make(map[string]string)
	sanitizedNames := make(map[string]int)
	functionDeclarations := make([][]byte, 0)
	hasStrictTool := false
	tools := gjson.GetBytes(rawJSON, "tools")
	toolResults := tools.Array()
	if tools.IsArray() && len(toolResults) > 0 {
		functionDeclarations = make([][]byte, 0, len(toolResults))
		googleSearchNodes := make([][]byte, 0)
		codeExecutionNodes := make([][]byte, 0)
		urlContextNodes := make([][]byte, 0)
		for _, t := range toolResults {
			if t.Get("type").String() == "function" {
				fn := t.Get("function")
				if fn.Exists() && fn.IsObject() {
					originalName := fn.Get("name").String()
					if isAllowedTools && !allowedToolNames[originalName] {
						continue
					}
					fnRaw := fn.Raw
					if fn.Get("parameters").Exists() {
						renamed, errRename := util.RenameKey(fnRaw, "parameters", "parametersJsonSchema")
						if errRename != nil {
							log.Warnf(
								"Failed to rename parameters for tool '%s': %v", fn.Get("name").String(), errRename,
							)
							var errSet error
							fnRawBytes := []byte(fnRaw)
							fnRawBytes, errSet = sjson.SetBytes(fnRawBytes, "parametersJsonSchema.type", "object")
							if errSet != nil {
								log.Warnf(
									"Failed to set default schema type for tool '%s': %v", fn.Get("name").String(),
									errSet,
								)
								continue
							}
							fnRawBytes, errSet = sjson.SetRawBytes(
								fnRawBytes, "parametersJsonSchema.properties", []byte(`{}`),
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
						fnRawBytes := []byte(fnRaw)
						fnRawBytes, errSet = sjson.SetBytes(fnRawBytes, "parametersJsonSchema.type", "object")
						if errSet != nil {
							log.Warnf(
								"Failed to set default schema type for tool '%s': %v", fn.Get("name").String(), errSet,
							)
							continue
						}
						fnRawBytes, errSet = sjson.SetRawBytes(
							fnRawBytes, "parametersJsonSchema.properties", []byte(`{}`),
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
					sanitizedName := util.SanitizeFunctionName(originalName)
					if nameResult.Type != gjson.String || sanitizedName != originalName {
						fnRawBytes, _ = sjson.SetBytes(fnRawBytes, "name", sanitizedName)
					}
					if parameters := gjson.GetBytes(fnRawBytes, "parametersJsonSchema"); parameters.Exists() {
						cleanedParameters := util.CleanJSONSchemaForGeminiJSONSchema(parameters.Raw)
						if cleanedParameters != parameters.Raw {
							fnRawBytes, _ = sjson.SetRawBytes(
								fnRawBytes,
								"parametersJsonSchema",
								[]byte(cleanedParameters),
							)
						}
					}
					if fn.Get("strict").Type == gjson.True || t.Get("strict").Type == gjson.True {
						hasStrictTool = true
					}
					if gjson.GetBytes(fnRawBytes, "strict").Exists() {
						fnRawBytes, _ = sjson.DeleteBytes(fnRawBytes, "strict")
					}
					declaredNames[originalName] = sanitizedName
					sanitizedNames[sanitizedName]++
					functionDeclarations = append(functionDeclarations, fnRawBytes)
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
		if len(functionDeclarations) > 0 ||
			len(googleSearchNodes) > 0 ||
			len(codeExecutionNodes) > 0 ||
			len(urlContextNodes) > 0 {
			toolItems := make(
				[][]byte,
				0,
				1+len(googleSearchNodes)+len(codeExecutionNodes)+len(urlContextNodes),
			)
			if len(functionDeclarations) > 0 {
				functionToolNode := []byte(`{"functionDeclarations":[]}`)
				functionToolNode, _ = sjson.SetRawBytes(
					functionToolNode,
					"functionDeclarations",
					translatorcommon.JoinRawArray(functionDeclarations),
				)
				toolItems = append(toolItems, functionToolNode)
			}
			toolItems = append(toolItems, googleSearchNodes...)
			toolItems = append(toolItems, codeExecutionNodes...)
			toolItems = append(toolItems, urlContextNodes...)
			out, _ = sjson.SetRawBytes(out, "tools", translatorcommon.JoinRawArray(toolItems))
		}
	}

	hasCollision := false
	for _, count := range sanitizedNames {
		if count > 1 {
			hasCollision = true
			break
		}
	}
	switch {
	case hasCollision:
		out, _ = sjson.SetBytes(out, "toolConfig.functionCallingConfig.mode", "NONE")
	case isAllowedTools:
		switch {
		case len(functionDeclarations) == 0:
			out, _ = sjson.SetBytes(out, "toolConfig.functionCallingConfig.mode", "NONE")
		case allowedMode == "required" || allowedMode == "any":
			out, _ = sjson.SetBytes(out, "toolConfig.functionCallingConfig.mode", "ANY")
			names := make([]string, 0, len(functionDeclarations))
			for _, declaration := range functionDeclarations {
				names = append(names, gjson.GetBytes(declaration, "name").String())
			}
			out, _ = sjson.SetBytes(out, "toolConfig.functionCallingConfig.allowedFunctionNames", names)
		case allowedMode == "" || allowedMode == "auto":
			mode := "AUTO"
			if hasStrictTool {
				mode = "VALIDATED"
			}
			out, _ = sjson.SetBytes(out, "toolConfig.functionCallingConfig.mode", mode)
		default:
			out, _ = sjson.SetBytes(out, "toolConfig.functionCallingConfig.mode", "NONE")
		}
	case toolChoice.Exists() && toolChoice.Type != gjson.Null:
		choiceType := strings.ToLower(strings.TrimSpace(toolChoice.String()))
		if toolChoice.IsObject() {
			choiceType = strings.ToLower(strings.TrimSpace(toolChoice.Get("type").String()))
		}
		switch choiceType {
		case "auto":
			mode := "AUTO"
			if hasStrictTool {
				mode = "VALIDATED"
			}
			out, _ = sjson.SetBytes(out, "toolConfig.functionCallingConfig.mode", mode)
		case "none":
			out, _ = sjson.SetBytes(out, "toolConfig.functionCallingConfig.mode", "NONE")
		case "required", "any":
			mode := "ANY"
			if len(functionDeclarations) == 0 {
				mode = "NONE"
			}
			out, _ = sjson.SetBytes(out, "toolConfig.functionCallingConfig.mode", mode)
		case "function", "tool":
			name := toolChoice.Get("function.name").String()
			if name == "" {
				name = toolChoice.Get("name").String()
			}
			sanitized, found := declaredNames[name]
			if !found || sanitizedNames[sanitized] != 1 {
				out, _ = sjson.SetBytes(out, "toolConfig.functionCallingConfig.mode", "NONE")
			} else {
				out, _ = sjson.SetBytes(out, "toolConfig.functionCallingConfig.mode", "ANY")
				out, _ = sjson.SetBytes(
					out, "toolConfig.functionCallingConfig.allowedFunctionNames", []string{sanitized},
				)
			}
		default:
			out, _ = sjson.SetBytes(out, "toolConfig.functionCallingConfig.mode", "NONE")
		}
	case hasStrictTool && len(functionDeclarations) > 0:
		out, _ = sjson.SetBytes(out, "toolConfig.functionCallingConfig.mode", "VALIDATED")
	}
	if gjson.GetBytes(rawJSON, "parallel_tool_calls").Type == gjson.False {
		out, _ = sjson.SetBytes(out, "toolConfig.functionCallingConfig.mode", "NONE")
		out, _ = sjson.DeleteBytes(out, "toolConfig.functionCallingConfig.allowedFunctionNames")
	}

	return common.AttachDefaultSafetySettings(out, "safetySettings"), drops.Err()
}

func applyOpenAIResponseFormatToGemini(out []byte, rawJSON []byte) []byte {
	responseFormat := gjson.GetBytes(rawJSON, "response_format")
	if !responseFormat.IsObject() {
		return out
	}

	switch strings.ToLower(strings.TrimSpace(responseFormat.Get("type").String())) {
	case "json_object":
		out, _ = sjson.SetBytes(out, "generationConfig.responseMimeType", "application/json")
		out, _ = sjson.DeleteBytes(out, "generationConfig.responseSchema")
		out, _ = sjson.DeleteBytes(out, "generationConfig.responseJsonSchema")
	case "json_schema":
		out, _ = sjson.SetBytes(out, "generationConfig.responseMimeType", "application/json")
		out, _ = sjson.DeleteBytes(out, "generationConfig.responseSchema")
		out, _ = sjson.DeleteBytes(out, "generationConfig.responseJsonSchema")

		schema := responseFormat.Get("json_schema.schema")
		if !schema.IsObject() {
			log.Warn("OpenAI response_format json_schema requires an object schema")
			return out
		}
		out, _ = sjson.SetRawBytes(out, "generationConfig.responseJsonSchema", []byte(schema.Raw))
	}

	return out
}

func parseOpenAIVideoDataURL(rawURL string) (string, string, bool) {
	dataURL, ok := strings.CutPrefix(strings.TrimSpace(rawURL), "data:")
	if !ok {
		return "", "", false
	}
	metadata, payload, ok := strings.Cut(dataURL, ",")
	if !ok || payload == "" {
		return "", "", false
	}
	metadataParts := strings.Split(metadata, ";")
	if len(metadataParts) < 2 || !strings.EqualFold(strings.TrimSpace(metadataParts[len(metadataParts)-1]), "base64") {
		return "", "", false
	}
	mediaType, _, errParseMediaType := mime.ParseMediaType(strings.Join(metadataParts[:len(metadataParts)-1], ";"))
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	if errParseMediaType != nil || !strings.HasPrefix(mediaType, "video/") || len(mediaType) == len("video/") {
		return "", "", false
	}
	decoded, errDecode := base64.StdEncoding.DecodeString(payload)
	if errDecode != nil {
		decoded, errDecode = base64.RawStdEncoding.DecodeString(payload)
	}
	if errDecode != nil || len(decoded) == 0 {
		return "", "", false
	}
	return mediaType, payload, true
}

// itoa converts int to string without strconv import for few usages.
func itoa(i int) string { return fmt.Sprintf("%d", i) }
