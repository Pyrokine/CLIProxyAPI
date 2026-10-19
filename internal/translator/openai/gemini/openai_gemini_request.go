// Package gemini provides request translation functionality for Gemini to OpenAI API.
// It handles parsing and transforming Gemini API requests into OpenAI Chat Completions API format,
// extracting model information, generation config, message contents, and tool declarations.
// The package performs JSON data transformation to ensure compatibility
// between Gemini API format and OpenAI API's expected format.
package gemini

import (
	"fmt"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ConvertGeminiRequestToOpenAI parses and transforms a Gemini API request into OpenAI Chat Completions API format.
// It extracts the model name, generation config, message contents, and tool declarations
// from the raw JSON request and returns them in the format expected by the OpenAI API.
func ConvertGeminiRequestToOpenAI(modelName string, inputRawJSON []byte, stream bool) []byte {
	rawJSON := inputRawJSON
	// Base OpenAI Chat Completions API template
	out := []byte(`{"model":"","messages":[]}`)

	root := gjson.ParseBytes(rawJSON)

	// Model mapping
	out, _ = sjson.SetBytes(out, "model", modelName)

	// Generation config mapping
	if genConfig := root.Get("generationConfig"); genConfig.Exists() {
		// Temperature
		if temp := genConfig.Get("temperature"); temp.Exists() {
			out, _ = sjson.SetBytes(out, "temperature", temp.Float())
		}

		// Max tokens
		if maxTokens := genConfig.Get("maxOutputTokens"); maxTokens.Exists() {
			out, _ = sjson.SetBytes(out, "max_tokens", maxTokens.Int())
		}

		// Top P
		if topP := genConfig.Get("topP"); topP.Exists() {
			out, _ = sjson.SetBytes(out, "top_p", topP.Float())
		}

		// Top K (OpenAI doesn't have direct equivalent, but we can map it)
		if topK := genConfig.Get("topK"); topK.Exists() {
			// Store as custom parameter for potential use
			out, _ = sjson.SetBytes(out, "top_k", topK.Int())
		}

		// Stop sequences
		if stopSequences := genConfig.Get("stopSequences"); stopSequences.Exists() && stopSequences.IsArray() {
			var stops []string
			stopSequences.ForEach(
				func(_, value gjson.Result) bool {
					stops = append(stops, value.String())
					return true
				},
			)
			if len(stops) > 0 {
				out, _ = sjson.SetBytes(out, "stop", stops)
			}
		}

		// Candidate count (OpenAI 'n' parameter)
		if candidateCount := genConfig.Get("candidateCount"); candidateCount.Exists() {
			out, _ = sjson.SetBytes(out, "n", candidateCount.Int())
		}

		// Map Gemini thinkingConfig to OpenAI reasoning_effort.
		// Always perform conversion to support allowCompat models that may not be in registry.
		// Note: Google official Python SDK sends snake_case fields (thinking_level/thinking_budget).
		if thinkingConfig := genConfig.Get("thinkingConfig"); thinkingConfig.Exists() && thinkingConfig.IsObject() {
			thinkingLevel := thinkingConfig.Get("thinkingLevel")
			if !thinkingLevel.Exists() {
				thinkingLevel = thinkingConfig.Get("thinking_level")
			}
			if thinkingLevel.Exists() {
				effort := strings.ToLower(strings.TrimSpace(thinkingLevel.String()))
				if effort != "" {
					out, _ = sjson.SetBytes(out, "reasoning_effort", effort)
				}
			} else {
				thinkingBudget := thinkingConfig.Get("thinkingBudget")
				if !thinkingBudget.Exists() {
					thinkingBudget = thinkingConfig.Get("thinking_budget")
				}
				if thinkingBudget.Exists() {
					if effort, ok := thinking.ConvertBudgetToLevel(int(thinkingBudget.Int())); ok {
						out, _ = sjson.SetBytes(out, "reasoning_effort", effort)
					}
				}
			}
		}
	}

	// Stream parameter
	out, _ = sjson.SetBytes(out, "stream", stream)

	// Process contents (Gemini messages) -> OpenAI messages
	var toolCalls translatorcommon.GeminiToolCallQueue
	toolCallCounter := 0
	newToolCallID := func() string {
		toolCallCounter++
		return fmt.Sprintf("call_gemini_%016d", toolCallCounter)
	}

	// System instruction -> OpenAI system message
	// Gemini may provide `systemInstruction` or `system_instruction`; support both keys.
	systemInstruction := root.Get("systemInstruction")
	if !systemInstruction.Exists() {
		systemInstruction = root.Get("system_instruction")
	}
	if systemInstruction.Exists() {
		parts := systemInstruction.Get("parts")
		msg := []byte(`{"role":"system","content":[]}`)
		hasContent := false

		if parts.Exists() && parts.IsArray() {
			parts.ForEach(
				func(_, part gjson.Result) bool {
					if translatorcommon.IsGeminiThoughtPart(part) {
						return true
					}

					// Handle text parts
					if text := part.Get("text"); text.Exists() {
						contentPart := []byte(`{"type":"text","text":""}`)
						contentPart, _ = sjson.SetBytes(contentPart, "text", text.String())
						msg, _ = sjson.SetRawBytes(msg, "content.-1", contentPart)
						hasContent = true
					}

					// Handle inline data (e.g., images)
					if contentPart := geminiInlineDataContentPart(part); contentPart != nil {
						msg, _ = sjson.SetRawBytes(msg, "content.-1", contentPart)
						hasContent = true
					}
					return true
				},
			)
		}

		if hasContent {
			out, _ = sjson.SetRawBytes(out, "messages.-1", msg)
		}
	}

	if contents := root.Get("contents"); contents.Exists() && contents.IsArray() {
		contents.ForEach(
			func(_, content gjson.Result) bool {
				role := content.Get("role").String()
				parts := content.Get("parts")

				// Convert role: model -> assistant
				if role == "model" {
					role = "assistant"
				}

				msg := []byte(`{"role":"","content":""}`)
				msg, _ = sjson.SetBytes(msg, "role", role)

				var textBuilder strings.Builder
				contentWrapper := []byte(`{"arr":[]}`)
				contentPartsCount := 0
				onlyTextContent := true
				toolCallsWrapper := []byte(`{"arr":[]}`)
				toolCallsCount := 0
				droppedThought := false

				if parts.Exists() && parts.IsArray() {
					parts.ForEach(
						func(_, part gjson.Result) bool {
							if translatorcommon.IsGeminiThoughtPart(part) {
								droppedThought = true
								return true
							}

							// Handle text parts
							if text := part.Get("text"); text.Exists() {
								formattedText := text.String()
								textBuilder.WriteString(formattedText)
								contentPart := []byte(`{"type":"text","text":""}`)
								contentPart, _ = sjson.SetBytes(contentPart, "text", formattedText)
								contentWrapper, _ = sjson.SetRawBytes(contentWrapper, "arr.-1", contentPart)
								contentPartsCount++
							}

							// Handle inline data (e.g., images)
							if contentPart := geminiInlineDataContentPart(part); contentPart != nil {
								onlyTextContent = false
								contentWrapper, _ = sjson.SetRawBytes(contentWrapper, "arr.-1", contentPart)
								contentPartsCount++
							}

							// Handle function calls (Gemini) -> tool calls (OpenAI)
							if functionCall := part.Get("functionCall"); functionCall.Exists() {
								functionName := functionCall.Get("name").String()
								argumentsRaw := ""
								if args := functionCall.Get("args"); args.Exists() {
									argumentsRaw = args.Raw
								}
								toolCallID := translatorcommon.GeminiToolCallID(functionCall)
								if toolCallID == "" {
									toolCallID = newToolCallID()
								}
								toolCalls.Add(functionName, toolCallID)

								toolCall := []byte(`{"id":"","type":"function","function":{"name":"","arguments":""}}`)
								toolCall, _ = sjson.SetBytes(toolCall, "id", toolCallID)
								toolCall, _ = sjson.SetBytes(toolCall, "function.name", functionName)
								if argumentsRaw != "" {
									toolCall, _ = sjson.SetBytes(toolCall, "function.arguments", argumentsRaw)
								} else {
									toolCall, _ = sjson.SetBytes(toolCall, "function.arguments", "{}")
								}

								toolCallsWrapper, _ = sjson.SetRawBytes(toolCallsWrapper, "arr.-1", toolCall)
								toolCallsCount++
							}

							// Handle function responses (Gemini) -> tool role messages (OpenAI)
							if functionResponse := part.Get("functionResponse"); functionResponse.Exists() {
								functionName := functionResponse.Get("name").String()
								toolMsg := []byte(`{"role":"tool","tool_call_id":"","content":""}`)

								responseRaw := ""
								if response := functionResponse.Get("response"); response.Exists() {
									if contentField := response.Get("content"); contentField.Exists() {
										responseRaw = contentField.Raw
									} else {
										responseRaw = response.Raw
									}
									toolMsg, _ = sjson.SetBytes(toolMsg, "content", responseRaw)
								}

								toolCallID := toolCalls.Match(functionName, translatorcommon.GeminiToolCallID(functionResponse))
								if toolCallID == "" {
									toolCallID = newToolCallID()
								}
								toolMsg, _ = sjson.SetBytes(toolMsg, "tool_call_id", toolCallID)
								out, _ = sjson.SetRawBytes(out, "messages.-1", toolMsg)
							}

							return true
						},
					)
				}

				// Set content
				if contentPartsCount > 0 {
					if onlyTextContent {
						msg, _ = sjson.SetBytes(msg, "content", textBuilder.String())
					} else {
						msg, _ = sjson.SetRawBytes(msg, "content", []byte(gjson.GetBytes(contentWrapper, "arr").Raw))
					}
				}

				// Set tool calls if any
				if toolCallsCount > 0 {
					msg, _ = sjson.SetRawBytes(msg, "tool_calls", []byte(gjson.GetBytes(toolCallsWrapper, "arr").Raw))
				}

				if droppedThought && contentPartsCount == 0 && toolCallsCount == 0 {
					return true
				}

				out, _ = sjson.SetRawBytes(out, "messages.-1", msg)
				return true
			},
		)
	}

	// Tools mapping: Gemini tools -> OpenAI tools
	if tools := root.Get("tools"); tools.Exists() && tools.IsArray() {
		tools.ForEach(
			func(_, tool gjson.Result) bool {
				if functionDeclarations := tool.Get("functionDeclarations"); functionDeclarations.Exists() && functionDeclarations.IsArray() {
					functionDeclarations.ForEach(
						func(_, funcDecl gjson.Result) bool {
							openAITool := []byte(`{"type":"function","function":{"name":"","description":""}}`)
							openAITool, _ = sjson.SetBytes(openAITool, "function.name", funcDecl.Get("name").String())
							openAITool, _ = sjson.SetBytes(
								openAITool, "function.description", funcDecl.Get("description").String(),
							)

							// Convert parameters schema
							if parameters := funcDecl.Get("parameters"); parameters.Exists() {
								openAITool, _ = sjson.SetRawBytes(
									openAITool, "function.parameters", []byte(parameters.Raw),
								)
							} else if parameters := funcDecl.Get("parametersJsonSchema"); parameters.Exists() {
								openAITool, _ = sjson.SetRawBytes(
									openAITool, "function.parameters", []byte(parameters.Raw),
								)
							}

							out, _ = sjson.SetRawBytes(out, "tools.-1", openAITool)
							return true
						},
					)
				}
				return true
			},
		)
	}

	// Tool choice mapping (Gemini doesn't have direct equivalent, but we can handle it)
	if toolConfig := root.Get("toolConfig"); toolConfig.Exists() {
		if functionCallingConfig := toolConfig.Get("functionCallingConfig"); functionCallingConfig.Exists() {
			mode := functionCallingConfig.Get("mode").String()
			switch mode {
			case "NONE":
				out, _ = sjson.SetBytes(out, "tool_choice", "none")
			case "AUTO":
				out, _ = sjson.SetBytes(out, "tool_choice", "auto")
			case "ANY":
				out, _ = sjson.SetBytes(out, "tool_choice", "required")
			}
		}
	}

	return out
}

func geminiInlineDataContentPart(part gjson.Result) []byte {
	inline := part.Get("inlineData")
	if !inline.Exists() {
		inline = part.Get("inline_data")
	}
	if !inline.Exists() {
		return nil
	}
	mimeType := inline.Get("mimeType").String()
	if mimeType == "" {
		mimeType = inline.Get("mime_type").String()
	}
	mimeType = strings.ToLower(strings.TrimSpace(mimeType))
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	data := inline.Get("data").String()
	dataURL := fmt.Sprintf("data:%s;base64,%s", mimeType, data)
	var out []byte
	switch {
	case strings.HasPrefix(mimeType, "image/"):
		out = []byte(`{"type":"image_url","image_url":{"url":""}}`)
		out, _ = sjson.SetBytes(out, "image_url.url", dataURL)
	case strings.HasPrefix(mimeType, "audio/"):
		format := strings.TrimPrefix(mimeType, "audio/")
		switch format {
		case "wave", "x-wav":
			format = "wav"
		case "mpeg":
			format = "mp3"
		case "ogg":
			format = "opus"
		case "pcm", "l16":
			format = "pcm16"
		}
		out = []byte(`{"type":"input_audio","input_audio":{"data":"","format":""}}`)
		out, _ = sjson.SetBytes(out, "input_audio.data", data)
		out, _ = sjson.SetBytes(out, "input_audio.format", format)
	case strings.HasPrefix(mimeType, "video/"):
		out = []byte(`{"type":"video_url","video_url":{"url":""}}`)
		out, _ = sjson.SetBytes(out, "video_url.url", dataURL)
	default:
		filename := "document.bin"
		switch mimeType {
		case "application/pdf":
			filename = "document.pdf"
		case "text/plain":
			filename = "document.txt"
		case "text/csv":
			filename = "document.csv"
		case "application/json":
			filename = "document.json"
		}
		out = []byte(`{"type":"file","file":{"filename":"","file_data":""}}`)
		out, _ = sjson.SetBytes(out, "file.filename", filename)
		out, _ = sjson.SetBytes(out, "file.file_data", dataURL)
	}
	return out
}
