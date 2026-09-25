// Package chat_completions provides response translation functionality for Gemini to OpenAI API compatibility.
// This package handles the conversion of Gemini API responses into OpenAI Chat Completions-compatible
// JSON format, transforming streaming events and non-streaming responses into the format
// expected by OpenAI API clients. It supports both streaming and non-streaming modes,
// handling text content, tool calls, reasoning content, and usage metadata appropriately.
package chat_completions

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	geminiopenai "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/gemini/openai"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// convertGeminiResponseToOpenAIChatParams holds parameters for response conversion.
type convertGeminiResponseToOpenAIChatParams struct {
	UnixTimestamp int64
	ResponseID    string
	Model         string
	Usage         geminiopenai.GeminiUsage
	// FunctionIndex tracks tool call indices per candidate index to support multiple candidates.
	FunctionIndex        map[int]int
	SawToolCall          map[int]bool
	UpstreamFinishReason map[int]string
}

// functionCallIDCounter provides a process-wide unique counter for function call identifiers.
var functionCallIDCounter atomic.Uint64

// convertGeminiResponseToOpenAI translates a single chunk of a streaming response from the
// Gemini API format to the OpenAI Chat Completions streaming format.
// It processes various Gemini event types and transforms them into OpenAI-compatible JSON responses.
// The function handles text content, tool calls, reasoning content, and usage metadata, outputting
// responses that match the OpenAI API format. It supports incremental updates for streaming responses.
//
// Parameters:
//   - ctx: The context for the request, used for cancellation and timeout handling
//   - modelName: The name of the model being used for the response (unused in current implementation)
//   - rawJSON: The raw JSON response from the Gemini API
//   - param: A pointer to a parameter object for maintaining state between calls
//
// Returns:
//   - [][]byte: A slice of OpenAI-compatible JSON responses
func convertGeminiResponseToOpenAI(_ context.Context, modelName string, _, _, rawJSON []byte, param *any) [][]byte {
	// Initialize parameters if nil.
	if *param == nil {
		*param = &convertGeminiResponseToOpenAIChatParams{
			UnixTimestamp:        0,
			FunctionIndex:        make(map[int]int),
			SawToolCall:          make(map[int]bool),
			UpstreamFinishReason: make(map[int]string),
		}
	}

	// Ensure the Map is initialized (handling cases where param might be reused from older context).
	p := (*param).(*convertGeminiResponseToOpenAIChatParams)
	if p.FunctionIndex == nil {
		p.FunctionIndex = make(map[int]int)
	}
	if p.SawToolCall == nil {
		p.SawToolCall = make(map[int]bool)
	}
	if p.UpstreamFinishReason == nil {
		p.UpstreamFinishReason = make(map[int]string)
	}

	if bytes.HasPrefix(rawJSON, []byte("data:")) {
		rawJSON = bytes.TrimSpace(rawJSON[5:])
	}

	if bytes.Equal(rawJSON, []byte("[DONE]")) {
		return [][]byte{}
	}

	root := unwrapGeminiChatResponseRoot(gjson.ParseBytes(rawJSON))
	if !root.Exists() {
		return [][]byte{}
	}
	if modelVersionResult := root.Get("modelVersion"); modelVersionResult.Type == gjson.String && modelVersionResult.String() != "" {
		p.Model = modelVersionResult.String()
	} else if p.Model == "" {
		p.Model = modelName
	}
	if responseIDResult := root.Get("responseId"); responseIDResult.Type == gjson.String && responseIDResult.String() != "" {
		p.ResponseID = responseIDResult.String()
	}
	if createTimeResult := root.Get("createTime"); createTimeResult.Type == gjson.String {
		if parsedTime, errParse := time.Parse(time.RFC3339Nano, createTimeResult.String()); errParse == nil {
			p.UnixTimestamp = parsedTime.Unix()
		}
	}
	p.Usage.Update(root.Get("usageMetadata"))

	// Initialize the OpenAI SSE base template.
	// We use a base template and clone it for each candidate to support multiple candidates.
	baseTemplate := []byte(`{"id":"","object":"chat.completion.chunk","created":0,"model":"","choices":[{"index":0,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":null},"finish_reason":null,"native_finish_reason":null}]}`)
	baseTemplate, _ = sjson.SetBytes(baseTemplate, "id", p.ResponseID)
	baseTemplate, _ = sjson.SetBytes(baseTemplate, "model", p.Model)
	baseTemplate, _ = sjson.SetBytes(baseTemplate, "created", p.UnixTimestamp)
	if p.Usage.Seen {
		baseTemplate, _ = sjson.SetBytes(baseTemplate, "usage.completion_tokens", p.Usage.OutputTokens())
		baseTemplate, _ = sjson.SetBytes(
			baseTemplate, "usage.completion_tokens_details.reasoning_tokens", p.Usage.ThoughtTokens,
		)
		baseTemplate, _ = sjson.SetBytes(baseTemplate, "usage.prompt_tokens", p.Usage.PromptTokens)
		baseTemplate, _ = sjson.SetBytes(
			baseTemplate, "usage.prompt_tokens_details.cached_tokens", p.Usage.CachedTokens,
		)
		baseTemplate, _ = sjson.SetBytes(baseTemplate, "usage.total_tokens", p.Usage.TotalTokens)
	}

	var responseStrings [][]byte
	candidates := root.Get("candidates")

	// Iterate over all candidates to support candidate_count > 1.
	if candidates.IsArray() {
		candidates.ForEach(
			func(_, candidate gjson.Result) bool {
				// Clone the template for the current candidate.
				template := append([]byte(nil), baseTemplate...)

				// Set the specific index for this candidate.
				candidateIndex := int(candidate.Get("index").Int())
				template, _ = sjson.SetBytes(template, "choices.0.index", candidateIndex)

				if finishReasonResult := candidate.Get("finishReason"); finishReasonResult.Exists() {
					p.UpstreamFinishReason[candidateIndex] = strings.ToUpper(finishReasonResult.String())
				}
				if stopReasonResult := root.Get("stop_reason"); stopReasonResult.Exists() && stopReasonResult.String() != "" {
					p.UpstreamFinishReason[candidateIndex] = strings.ToUpper(stopReasonResult.String())
				}

				partsResult := candidate.Get("content.parts")
				assistantRoleSet := false
				setAssistantRole := func() {
					if assistantRoleSet {
						return
					}
					template, _ = sjson.SetBytes(template, "choices.0.delta.role", "assistant")
					assistantRoleSet = true
				}

				if partsResult.IsArray() {
					partResults := partsResult.Array()
					for i := range partResults {
						partResult := partResults[i]
						partTextResult := partResult.Get("text")
						if !partTextResult.Exists() {
							partTextResult = partResult.Get("audioTranscription.text")
						}
						functionCallResult := partResult.Get("functionCall")
						inlineDataResult := partResult.Get("inlineData")
						if !inlineDataResult.Exists() {
							inlineDataResult = partResult.Get("inline_data")
						}
						thoughtSignatureResult := partResult.Get("thoughtSignature")
						if !thoughtSignatureResult.Exists() {
							thoughtSignatureResult = partResult.Get("thought_signature")
						}

						hasThoughtSignature := thoughtSignatureResult.Exists() && thoughtSignatureResult.String() != ""
						hasContentPayload := partTextResult.Exists() || functionCallResult.Exists() || inlineDataResult.Exists()

						// Skip pure thoughtSignature parts but keep any actual payload in the same part.
						if hasThoughtSignature && !hasContentPayload {
							continue
						}

						if partTextResult.Exists() {
							text := partTextResult.String()
							setAssistantRole()
							// Handle text content, distinguishing between regular content and reasoning/thoughts.
							if partResult.Get("thought").Bool() {
								oldValue := gjson.GetBytes(template, "choices.0.delta.reasoning_content").String()
								template, _ = sjson.SetBytes(template, "choices.0.delta.reasoning_content", oldValue+text)
							} else {
								oldValue := gjson.GetBytes(template, "choices.0.delta.content").String()
								template, _ = sjson.SetBytes(template, "choices.0.delta.content", oldValue+text)
							}
						} else if functionCallResult.Exists() {
							// Handle function call content.
							p.SawToolCall[candidateIndex] = true
							toolCallsResult := gjson.GetBytes(template, "choices.0.delta.tool_calls")

							// Retrieve the function index for this specific candidate.
							functionCallIndex := p.FunctionIndex[candidateIndex]
							p.FunctionIndex[candidateIndex]++

							if toolCallsResult.Exists() && toolCallsResult.IsArray() {
								functionCallIndex = len(toolCallsResult.Array())
							} else {
								template, _ = sjson.SetRawBytes(template, "choices.0.delta.tool_calls", []byte(`[]`))
							}

							functionCallTemplate := []byte(`{"id":"","index":0,"type":"function","function":{"name":"","arguments":""}}`)
							fcName := functionCallResult.Get("name").String()
							functionCallTemplate, _ = sjson.SetBytes(
								functionCallTemplate, "id", fmt.Sprintf(
									"%s-%d-%d", fcName, time.Now().UnixNano(),
									functionCallIDCounter.Add(1),
								),
							)
							functionCallTemplate, _ = sjson.SetBytes(functionCallTemplate, "index", functionCallIndex)
							functionCallTemplate, _ = sjson.SetBytes(functionCallTemplate, "function.name", fcName)
							if fcArgsResult := functionCallResult.Get("args"); fcArgsResult.Exists() {
								functionCallTemplate, _ = sjson.SetBytes(
									functionCallTemplate, "function.arguments", fcArgsResult.Raw,
								)
							}
							setAssistantRole()
							template, _ = sjson.SetRawBytes(
								template, "choices.0.delta.tool_calls.-1", functionCallTemplate,
							)
						} else if inlineDataResult.Exists() {
							data := inlineDataResult.Get("data").String()
							if data == "" {
								continue
							}
							mimeType := inlineDataResult.Get("mimeType").String()
							if mimeType == "" {
								mimeType = inlineDataResult.Get("mime_type").String()
							}
							if mimeType == "" {
								mimeType = "image/png"
							}
							imageURL := fmt.Sprintf("data:%s;base64,%s", mimeType, data)
							imagesResult := gjson.GetBytes(template, "choices.0.delta.images")
							if !imagesResult.Exists() || !imagesResult.IsArray() {
								template, _ = sjson.SetRawBytes(template, "choices.0.delta.images", []byte(`[]`))
							}
							imageIndex := len(gjson.GetBytes(template, "choices.0.delta.images").Array())
							imagePayload := []byte(`{"type":"image_url","image_url":{"url":""}}`)
							imagePayload, _ = sjson.SetBytes(imagePayload, "index", imageIndex)
							imagePayload, _ = sjson.SetBytes(imagePayload, "image_url.url", imageURL)
							setAssistantRole()
							template, _ = sjson.SetRawBytes(template, "choices.0.delta.images.-1", imagePayload)
						}
					}
				}

				upstreamFinishReason := p.UpstreamFinishReason[candidateIndex]
				sawToolCall := p.SawToolCall[candidateIndex]
				isFinalChunk := upstreamFinishReason != "" && p.Usage.Seen

				if isFinalChunk {
					finishReason := "stop"
					if sawToolCall {
						finishReason = "tool_calls"
					} else if upstreamFinishReason == "MAX_TOKENS" {
						finishReason = "max_tokens"
					}
					template, _ = sjson.SetBytes(template, "choices.0.finish_reason", finishReason)
					template, _ = sjson.SetBytes(
						template, "choices.0.native_finish_reason", strings.ToLower(upstreamFinishReason),
					)
				}

				responseStrings = append(responseStrings, template)
				return true // continue loop
			},
		)
	} else {
		// If there are no candidates (e.g., a pure usageMetadata chunk), return the usage chunk if present.
		if root.Get("usageMetadata").Exists() && len(responseStrings) == 0 {
			responseStrings = append(responseStrings, append([]byte(nil), baseTemplate...))
		}
	}

	return responseStrings
}

// ConvertGeminiResponseToOpenAINonStream converts a non-streaming Gemini response to a non-streaming OpenAI response.
// This function processes the complete Gemini response and transforms it into a single OpenAI-compatible
// JSON response. It handles message content, tool calls, reasoning content, and usage metadata, combining all
// the information into a single response that matches the OpenAI API format.
//
// Parameters:
//   - ctx: The context for the request, used for cancellation and timeout handling
//   - modelName: The name of the model being used for the response (unused in current implementation)
//   - rawJSON: The raw JSON response from the Gemini API
//   - param: A pointer to a parameter object for the conversion (unused in current implementation)
//
// Returns:
//   - []byte: An OpenAI-compatible JSON response containing all message content and metadata
func ConvertGeminiResponseToOpenAINonStream(
	_ context.Context,
	modelName string,
	_, _, rawJSON []byte,
	_ *any,
) []byte {
	root := unwrapGeminiChatResponseRoot(gjson.ParseBytes(rawJSON))
	var unixTimestamp int64
	// Initialize template with an empty choices array to support multiple candidates.
	template := []byte(`{"id":"","object":"chat.completion","created":0,"model":"","choices":[]}`)

	model := modelName
	if modelVersionResult := root.Get("modelVersion"); modelVersionResult.Type == gjson.String && modelVersionResult.String() != "" {
		model = modelVersionResult.String()
	}
	template, _ = sjson.SetBytes(template, "model", model)

	if createTimeResult := root.Get("createTime"); createTimeResult.Type == gjson.String {
		if parsedTime, errParse := time.Parse(time.RFC3339Nano, createTimeResult.String()); errParse == nil {
			unixTimestamp = parsedTime.Unix()
		}
	}
	template, _ = sjson.SetBytes(template, "created", unixTimestamp)

	if responseIDResult := root.Get("responseId"); responseIDResult.Type == gjson.String {
		template, _ = sjson.SetBytes(template, "id", responseIDResult.String())
	}

	var usage geminiopenai.GeminiUsage
	usage.Update(root.Get("usageMetadata"))
	if usage.Seen {
		template, _ = sjson.SetBytes(template, "usage.completion_tokens", usage.OutputTokens())
		template, _ = sjson.SetBytes(
			template, "usage.completion_tokens_details.reasoning_tokens", usage.ThoughtTokens,
		)
		template, _ = sjson.SetBytes(template, "usage.prompt_tokens", usage.PromptTokens)
		template, _ = sjson.SetBytes(template, "usage.prompt_tokens_details.cached_tokens", usage.CachedTokens)
		template, _ = sjson.SetBytes(template, "usage.total_tokens", usage.TotalTokens)
	}

	// Process the main content part of the response for all candidates.
	candidates := root.Get("candidates")
	if candidates.IsArray() {
		candidates.ForEach(
			func(_, candidate gjson.Result) bool {
				// Construct a single Choice object.
				choiceTemplate := []byte(`{"index":0,"message":{"role":"assistant","content":null,"reasoning_content":null,"tool_calls":null},"finish_reason":null,"native_finish_reason":null}`)

				// Set the index for this choice.
				choiceTemplate, _ = sjson.SetBytes(choiceTemplate, "index", candidate.Get("index").Int())

				// Set finish reason.
				if finishReasonResult := candidate.Get("finishReason"); finishReasonResult.Exists() {
					choiceTemplate, _ = sjson.SetBytes(
						choiceTemplate, "finish_reason", strings.ToLower(finishReasonResult.String()),
					)
					choiceTemplate, _ = sjson.SetBytes(
						choiceTemplate, "native_finish_reason", strings.ToLower(finishReasonResult.String()),
					)
				}

				partsResult := candidate.Get("content.parts")
				hasFunctionCall := false
				if partsResult.IsArray() {
					partsResults := partsResult.Array()
					for i := range partsResults {
						partResult := partsResults[i]
						partTextResult := partResult.Get("text")
						if !partTextResult.Exists() {
							partTextResult = partResult.Get("audioTranscription.text")
						}
						functionCallResult := partResult.Get("functionCall")
						inlineDataResult := partResult.Get("inlineData")
						if !inlineDataResult.Exists() {
							inlineDataResult = partResult.Get("inline_data")
						}

						if partTextResult.Exists() {
							// Append text content, distinguishing between regular content and reasoning.
							if partResult.Get("thought").Bool() {
								oldVal := gjson.GetBytes(choiceTemplate, "message.reasoning_content").String()
								choiceTemplate, _ = sjson.SetBytes(
									choiceTemplate, "message.reasoning_content", oldVal+partTextResult.String(),
								)
							} else {
								oldVal := gjson.GetBytes(choiceTemplate, "message.content").String()
								choiceTemplate, _ = sjson.SetBytes(
									choiceTemplate, "message.content", oldVal+partTextResult.String(),
								)
							}
						} else if functionCallResult.Exists() {
							// Append function call content to the tool_calls array.
							hasFunctionCall = true
							toolCallsResult := gjson.GetBytes(choiceTemplate, "message.tool_calls")
							if !toolCallsResult.Exists() || !toolCallsResult.IsArray() {
								choiceTemplate, _ = sjson.SetRawBytes(
									choiceTemplate, "message.tool_calls", []byte(`[]`),
								)
							}
							functionCallItemTemplate := []byte(`{"id":"","type":"function","function":{"name":"","arguments":""}}`)
							fcName := functionCallResult.Get("name").String()
							functionCallItemTemplate, _ = sjson.SetBytes(
								functionCallItemTemplate, "id", fmt.Sprintf(
									"%s-%d-%d", fcName, time.Now().UnixNano(),
									functionCallIDCounter.Add(1),
								),
							)
							functionCallItemTemplate, _ = sjson.SetBytes(
								functionCallItemTemplate, "function.name", fcName,
							)
							if fcArgsResult := functionCallResult.Get("args"); fcArgsResult.Exists() {
								functionCallItemTemplate, _ = sjson.SetBytes(
									functionCallItemTemplate, "function.arguments", fcArgsResult.Raw,
								)
							}
							choiceTemplate, _ = sjson.SetRawBytes(
								choiceTemplate, "message.tool_calls.-1", functionCallItemTemplate,
							)
						} else if inlineDataResult.Exists() {
							data := inlineDataResult.Get("data").String()
							if data != "" {
								mimeType := inlineDataResult.Get("mimeType").String()
								if mimeType == "" {
									mimeType = inlineDataResult.Get("mime_type").String()
								}
								if mimeType == "" {
									mimeType = "image/png"
								}
								imageURL := fmt.Sprintf("data:%s;base64,%s", mimeType, data)
								imagesResult := gjson.GetBytes(choiceTemplate, "message.images")
								if !imagesResult.Exists() || !imagesResult.IsArray() {
									choiceTemplate, _ = sjson.SetRawBytes(
										choiceTemplate, "message.images", []byte(`[]`),
									)
								}
								imageIndex := len(gjson.GetBytes(choiceTemplate, "message.images").Array())
								imagePayload := []byte(`{"type":"image_url","image_url":{"url":""}}`)
								imagePayload, _ = sjson.SetBytes(imagePayload, "index", imageIndex)
								imagePayload, _ = sjson.SetBytes(imagePayload, "image_url.url", imageURL)
								choiceTemplate, _ = sjson.SetRawBytes(choiceTemplate, "message.images.-1", imagePayload)
							}
						}
					}
				}

				if hasFunctionCall {
					choiceTemplate, _ = sjson.SetBytes(choiceTemplate, "finish_reason", "tool_calls")
					choiceTemplate, _ = sjson.SetBytes(choiceTemplate, "native_finish_reason", "tool_calls")
				}

				// Append the constructed choice to the main choices array.
				template, _ = sjson.SetRawBytes(template, "choices.-1", choiceTemplate)
				return true
			},
		)
	}

	return template
}

func unwrapGeminiChatResponseRoot(root gjson.Result) gjson.Result {
	response := root.Get("response")
	if response.IsObject() && (response.Get("candidates").Exists() || response.Get("responseId").Exists() ||
		response.Get("usageMetadata").Exists() || response.Get("error").Exists()) {
		return response
	}
	return root
}
