// Package chat_completions provides request translation functionality for OpenAI to Gemini API compatibility.
// It converts OpenAI Chat Completions requests into Gemini compatible JSON using gjson/sjson only.
package chat_completions

import (
	"fmt"
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
func convertOpenAIRequestToGemini(modelName string, inputRawJSON []byte, _ bool) []byte {
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
		// First pass: assistant tool_calls id->name map
		tcID2Name := map[string]string{}
		for i := range arr {
			m := arr[i]
			if m.Get("role").String() == "assistant" {
				tcs := m.Get("tool_calls")
				if tcs.IsArray() {
					for _, tc := range tcs.Array() {
						if tc.Get("type").String() == "function" {
							id := tc.Get("id").String()
							name := tc.Get("function.name").String()
							if id != "" && name != "" {
								tcID2Name[id] = name
							}
						}
					}
				}
			}
		}

		// Second pass build systemInstruction/tool responses cache
		toolResponses := map[string]string{} // tool_call_id -> response text
		for i := range arr {
			m := arr[i]
			role := m.Get("role").String()
			if role == "tool" {
				toolCallID := m.Get("tool_call_id").String()
				if toolCallID != "" {
					c := m.Get("content")
					toolResponses[toolCallID] = c.Raw
				}
			}
		}

		systemPartIndex := 0
		for i := range arr {
			m := arr[i]
			role := m.Get("role").String()
			content := m.Get("content")

			if (role == "system" || role == "developer") && len(arr) > 1 {
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
			} else if role == "user" || ((role == "system" || role == "developer") && len(arr) == 1) {
				// Build single user content node to avoid splitting into multiple contents
				node := []byte(`{"role":"user","parts":[]}`)
				if content.Type == gjson.String {
					node, _ = sjson.SetBytes(node, "parts.0.text", content.String())
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
							imageURL := item.Get("image_url.url").String()
							if len(imageURL) > 5 {
								pieces := strings.SplitN(imageURL[5:], ";", 2)
								if len(pieces) == 2 && len(pieces[1]) > 7 {
									mime := pieces[0]
									data := pieces[1][7:]
									node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".inlineData.mime_type", mime)
									node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".inlineData.data", data)
									node, _ = sjson.SetBytes(
										node, "parts."+itoa(p)+".thoughtSignature", geminiFunctionThoughtSignature,
									)
									p++
								}
							}
						case "video_url":
							videoURL := item.Get("video_url.url").String()
							if strings.HasPrefix(videoURL, "data:") {
								pieces := strings.SplitN(videoURL[5:], ";", 2)
								if len(pieces) == 2 && strings.HasPrefix(pieces[1], "base64,") {
									mime := pieces[0]
									data := strings.TrimPrefix(pieces[1], "base64,")
									node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".inlineData.mime_type", mime)
									node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".inlineData.data", data)
									node, _ = sjson.SetBytes(
										node, "parts."+itoa(p)+".thoughtSignature", geminiFunctionThoughtSignature,
									)
									p++
								}
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
							}
						}
					}
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
									node, _ = sjson.SetBytes(
										node, "parts."+itoa(p)+".thoughtSignature", geminiFunctionThoughtSignature,
									)
									p++
								}
							}
						}
					}
				}

				// Tool calls -> single model content with functionCall parts
				tcs := m.Get("tool_calls")
				if tcs.IsArray() {
					fIDs := make([]string, 0)
					for _, tc := range tcs.Array() {
						if tc.Get("type").String() != "function" {
							continue
						}
						fid := tc.Get("id").String()
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
						if fid != "" {
							fIDs = append(fIDs, fid)
						}
					}
					if p > 0 {
						out, _ = sjson.SetRawBytes(out, "contents.-1", node)
					}

					// Append a single tool content combining name + response per function
					toolNode := []byte(`{"role":"user","parts":[]}`)
					pp := 0
					for _, fid := range fIDs {
						if name, ok := tcID2Name[fid]; ok {
							toolNode, _ = sjson.SetBytes(toolNode, "parts."+itoa(pp)+".functionResponse.name", name)
							resp := toolResponses[fid]
							if resp == "" {
								resp = "{}"
							}
							toolNode, _ = sjson.SetBytes(
								toolNode, "parts."+itoa(pp)+".functionResponse.response.result", []byte(resp),
							)
							pp++
						}
					}
					if pp > 0 {
						out, _ = sjson.SetRawBytes(out, "contents.-1", toolNode)
					}
				} else if p > 0 {
					out, _ = sjson.SetRawBytes(out, "contents.-1", node)
				}
			}
		}
	}

	// tools -> tools[].functionDeclarations + tools[].googleSearch/codeExecution/urlContext passthrough
	tools := gjson.GetBytes(rawJSON, "tools")
	toolResults := tools.Array()
	if tools.IsArray() && len(toolResults) > 0 {
		functionDeclarations := make([][]byte, 0, len(toolResults))
		googleSearchNodes := make([][]byte, 0)
		codeExecutionNodes := make([][]byte, 0)
		urlContextNodes := make([][]byte, 0)
		for _, t := range toolResults {
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
					originalName := nameResult.String()
					sanitizedName := util.SanitizeFunctionName(originalName)
					if nameResult.Type != gjson.String || sanitizedName != originalName {
						fnRawBytes, _ = sjson.SetBytes(fnRawBytes, "name", sanitizedName)
					}
					if parameters := gjson.GetBytes(fnRawBytes, "parametersJsonSchema"); parameters.Exists() {
						cleanedParameters := util.CleanJSONSchemaForGemini(parameters.Raw)
						if cleanedParameters != parameters.Raw {
							fnRawBytes, _ = sjson.SetRawBytes(
								fnRawBytes,
								"parametersJsonSchema",
								[]byte(cleanedParameters),
							)
						}
					}
					if gjson.GetBytes(fnRawBytes, "strict").Exists() {
						fnRawBytes, _ = sjson.DeleteBytes(fnRawBytes, "strict")
					}
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

	return common.AttachDefaultSafetySettings(out, "safetySettings")
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

// itoa converts int to string without strconv import for few usages.
func itoa(i int) string { return fmt.Sprintf("%d", i) }
