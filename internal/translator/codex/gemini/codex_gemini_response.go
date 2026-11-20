// Package gemini provides response translation functionality for Codex to Gemini API compatibility.
// This package handles the conversion of Codex API responses into Gemini-compatible
// JSON format, transforming streaming events and non-streaming responses into the format
// expected by Gemini API clients.
//
//goland:noinspection GrazieInspection
package gemini

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var (
	dataTag = []byte("data:")
)

// convertCodexResponseToGeminiParams holds parameters for response conversion.
type convertCodexResponseToGeminiParams struct {
	Model              string
	CreatedAt          int64
	ResponseID         string
	LastStorageOutput  []byte
	HasOutputTextDelta bool
	ImageResults       map[string]string
}

// ConvertCodexResponseToGemini converts Codex streaming response format to Gemini format.
// This function processes various Codex event types and transforms them into Gemini-compatible JSON responses.
// It handles text content, tool calls, and usage metadata, outputting responses that match the Gemini API format.
// The function maintains state across multiple calls to ensure proper response sequencing.
//
// Parameters:
//   - ctx: The context for the request, used for cancellation and timeout handling
//   - modelName: The name of the model being used for the response
//   - rawJSON: The raw JSON response from the Codex API
//   - param: A pointer to a parameter object for maintaining state between calls
//
// Returns:
//   - [][]byte: A slice of Gemini-compatible JSON responses
func ConvertCodexResponseToGemini(
	_ context.Context,
	modelName string,
	originalRequestRawJSON, _, rawJSON []byte,
	param *any,
) [][]byte {
	if *param == nil {
		*param = &convertCodexResponseToGeminiParams{
			Model:              modelName,
			CreatedAt:          0,
			ResponseID:         "",
			LastStorageOutput:  nil,
			HasOutputTextDelta: false,
		}
	}

	if !bytes.HasPrefix(rawJSON, dataTag) {
		return [][]byte{}
	}
	rawJSON = bytes.TrimSpace(rawJSON[5:])

	rootResult := gjson.ParseBytes(rawJSON)
	typeResult := rootResult.Get("type")
	typeStr := typeResult.String()
	params := (*param).(*convertCodexResponseToGeminiParams)

	// Base Gemini response template
	template := []byte(`{"candidates":[{"content":{"role":"model","parts":[]}}],"usageMetadata` +
		`":{"trafficType":"PROVISIONED_THROUGHPUT"},"modelVersion":"gemini-2.5-` +
		`pro","createTime":"2025-08-15T02:52:03.884209Z","responseId":"06CeaPH7` +
		`NaCU48APvNXDyA4"}`)
	template, _ = sjson.SetBytes(template, "modelVersion", params.Model)
	createdAtResult := rootResult.Get("response.created_at")
	if createdAtResult.Exists() {
		params.CreatedAt = createdAtResult.Int()
		template, _ = sjson.SetBytes(
			template, "createTime",
			time.Unix(params.CreatedAt, 0).Format(time.RFC3339Nano),
		)
	}
	template, _ = sjson.SetBytes(template, "responseId", params.ResponseID)

	emitImage := func(event, item gjson.Result, field string) {
		part := codexGeminiImage(item, field)
		if len(part) == 0 {
			return
		}
		key := item.Get("id").String()
		if key == "" {
			key = event.Get("item_id").String()
		}
		if key == "" {
			key = "output:" + event.Get("output_index").Raw
		}
		data := gjson.GetBytes(part, "inlineData.data").String()
		if params.ImageResults[key] == data {
			return
		}
		if params.ImageResults == nil {
			params.ImageResults = make(map[string]string)
		}
		params.ImageResults[key] = data
		template, _ = sjson.SetRawBytes(template, "candidates.0.content.parts.-1", part)
	}
	if typeStr == "response.image_generation_call.partial_image" {
		emitImage(rootResult, rootResult, "partial_image_b64")
		if len(gjson.GetBytes(template, "candidates.0.content.parts").Array()) == 0 {
			return nil
		}
		return [][]byte{template}
	}
	if typeStr == "response.output_item.done" &&
		rootResult.Get("item.type").String() == "image_generation_call" {
		emitImage(rootResult, rootResult.Get("item"), "result")
		if len(gjson.GetBytes(template, "candidates.0.content.parts").Array()) == 0 {
			return nil
		}
		return [][]byte{template}
	}

	// Handle function call completion
	if typeStr == "response.output_item.done" {
		itemResult := rootResult.Get("item")
		itemType := itemResult.Get("type").String()
		if itemType == "function_call" {
			// Create function call part
			functionCall := []byte(`{"functionCall":{"name":"","args":{}}}`)
			if id := itemResult.Get("call_id").String(); id != "" {
				functionCall, _ = sjson.SetBytes(functionCall, "functionCall.id", id)
			}
			{
				// Restore original tool name if shortened
				n := itemResult.Get("name").String()
				rev := buildReverseMapFromGeminiOriginal(originalRequestRawJSON)
				if orig, ok := rev[n]; ok {
					n = orig
				}
				functionCall, _ = sjson.SetBytes(functionCall, "functionCall.name", n)
			}

			// Parse and set arguments
			argsStr := itemResult.Get("arguments").String()
			if argsStr != "" {
				argsResult := gjson.Parse(argsStr)
				if argsResult.IsObject() {
					functionCall, _ = sjson.SetRawBytes(functionCall, "functionCall.args", []byte(argsStr))
				}
			}

			template, _ = sjson.SetRawBytes(template, "candidates.0.content.parts.-1", functionCall)
			template, _ = sjson.SetBytes(template, "candidates.0.finishReason", "STOP")

			params.LastStorageOutput = append([]byte(nil), template...)

			// Use this return to storage message
			return [][]byte{}
		}
	}

	if typeStr == "response.created" { // Handle response creation - set model and response ID
		template, _ = sjson.SetBytes(template, "modelVersion", rootResult.Get("response.model").String())
		template, _ = sjson.SetBytes(template, "responseId", rootResult.Get("response.id").String())
		params.ResponseID = rootResult.Get("response.id").String()
	} else if typeStr == "response.reasoning_summary_text.delta" { // Handle reasoning/thinking content delta
		part := []byte(`{"thought":true,"text":""}`)
		part, _ = sjson.SetBytes(part, "text", rootResult.Get("delta").String())
		template, _ = sjson.SetRawBytes(template, "candidates.0.content.parts.-1", part)
	} else if typeStr == "response.output_text.delta" { // Handle regular text content delta
		params.HasOutputTextDelta = true
		part := []byte(`{"text":""}`)
		part, _ = sjson.SetBytes(part, "text", rootResult.Get("delta").String())
		template, _ = sjson.SetRawBytes(template, "candidates.0.content.parts.-1", part)
	} else if typeStr == "response.output_item.done" {
		itemResult := rootResult.Get("item")
		if itemResult.Get("type").String() != "message" || params.HasOutputTextDelta {
			return [][]byte{}
		}
		contentResult := itemResult.Get("content")
		if !contentResult.Exists() || !contentResult.IsArray() {
			return [][]byte{}
		}
		wroteText := false
		contentResult.ForEach(
			func(_, partResult gjson.Result) bool {
				if partResult.Get("type").String() != "output_text" {
					return true
				}
				text := partResult.Get("text").String()
				if text == "" {
					return true
				}
				part := []byte(`{"text":""}`)
				part, _ = sjson.SetBytes(part, "text", text)
				template, _ = sjson.SetRawBytes(template, "candidates.0.content.parts.-1", part)
				wroteText = true
				return true
			},
		)
		if wroteText {
			params.HasOutputTextDelta = true
			return [][]byte{template}
		}
		return [][]byte{}
	} else if typeStr == "response.completed" || typeStr == "response.incomplete" {
		for index, item := range rootResult.Get("response.output").Array() {
			if item.Get("type").String() == "image_generation_call" {
				event := gjson.Parse(`{"output_index":` + fmt.Sprint(index) + `}`)
				emitImage(event, item, "result")
			}
		}
		template, _ = sjson.SetBytes(
			template, "usageMetadata.promptTokenCount", rootResult.Get("response.usage.input_tokens").Int(),
		)
		template, _ = sjson.SetBytes(
			template, "usageMetadata.candidatesTokenCount", rootResult.Get("response.usage.output_tokens").Int(),
		)
		totalTokens := rootResult.Get("response.usage.input_tokens").Int() +
			rootResult.Get("response.usage.output_tokens").Int()
		template, _ = sjson.SetBytes(template, "usageMetadata.totalTokenCount", totalTokens)
		if typeStr == "response.incomplete" {
			template, _ = sjson.SetBytes(
				template,
				"candidates.0.finishReason",
				codexGeminiIncompleteFinishReason(rootResult.Get("response.incomplete_details.reason").String()),
			)
		}
	} else {
		return [][]byte{}
	}

	if len(params.LastStorageOutput) > 0 {
		stored := append([]byte(nil), params.LastStorageOutput...)
		params.LastStorageOutput = nil
		return [][]byte{stored, template}
	}
	return [][]byte{template}
}

// ConvertCodexResponseToGeminiNonStream converts a non-streaming Codex response to a non-streaming Gemini response.
// This function processes the complete Codex response and transforms it into a single Gemini-compatible
// JSON response. It handles message content, tool calls, reasoning content, and usage metadata, combining all
// the information into a single response that matches the Gemini API format.
//
// Parameters:
//   - ctx: The context for the request, used for cancellation and timeout handling
//   - modelName: The name of the model being used for the response
//   - rawJSON: The raw JSON response from the Codex API
//   - param: A pointer to a parameter object for the conversion (unused in current implementation)
//
// Returns:
//   - []byte: A Gemini-compatible JSON response containing all message content and metadata
func ConvertCodexResponseToGeminiNonStream(
	_ context.Context,
	modelName string,
	originalRequestRawJSON, _, rawJSON []byte,
	_ *any,
) []byte {
	rootResult := gjson.ParseBytes(rawJSON)

	responseType := rootResult.Get("type").String()
	if responseType != "response.completed" && responseType != "response.incomplete" {
		return []byte{}
	}

	// Base Gemini response template for non-streaming
	template := []byte(`{"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"` +
		`STOP"}],"usageMetadata":{"trafficType":"PROVISIONED_THROUGHPUT"},"mode` +
		`lVersion":"","createTime":"","responseId":""}`)

	// Set model version
	template, _ = sjson.SetBytes(template, "modelVersion", modelName)

	// Set response metadata from the completed response
	responseData := rootResult.Get("response")
	if responseData.Exists() {
		if responseType == "response.incomplete" {
			template, _ = sjson.SetBytes(
				template,
				"candidates.0.finishReason",
				codexGeminiIncompleteFinishReason(responseData.Get("incomplete_details.reason").String()),
			)
		}
		// Set response ID
		if responseId := responseData.Get("id"); responseId.Exists() {
			template, _ = sjson.SetBytes(template, "responseId", responseId.String())
		}

		// Set creation time
		if createdAt := responseData.Get("created_at"); createdAt.Exists() {
			template, _ = sjson.SetBytes(
				template,
				"createTime",
				time.Unix(createdAt.Int(), 0).Format(time.RFC3339Nano),
			)
		}

		// Set usage metadata
		if usage := responseData.Get("usage"); usage.Exists() {
			inputTokens := usage.Get("input_tokens").Int()
			outputTokens := usage.Get("output_tokens").Int()
			totalTokens := inputTokens + outputTokens

			template, _ = sjson.SetBytes(template, "usageMetadata.promptTokenCount", inputTokens)
			template, _ = sjson.SetBytes(template, "usageMetadata.candidatesTokenCount", outputTokens)
			template, _ = sjson.SetBytes(template, "usageMetadata.totalTokenCount", totalTokens)
		}

		// Process output content to build parts array
		var pendingFunctionCalls [][]byte

		flushPendingFunctionCalls := func() {
			if len(pendingFunctionCalls) == 0 {
				return
			}
			// Add all pending function calls as individual parts
			// This maintains the original Gemini API format while ensuring consecutive calls are grouped together
			for _, fc := range pendingFunctionCalls {
				template, _ = sjson.SetRawBytes(template, "candidates.0.content.parts.-1", fc)
			}
			pendingFunctionCalls = nil
		}

		if output := responseData.Get("output"); output.Exists() && output.IsArray() {
			output.ForEach(
				func(key, value gjson.Result) bool {
					itemType := value.Get("type").String()

					switch itemType {
					case "reasoning":
						// Flush any pending function calls before adding non-function content
						flushPendingFunctionCalls()

						// Add thinking content
						if content := value.Get("content"); content.Exists() {
							part := []byte(`{"text":"","thought":true}`)
							part, _ = sjson.SetBytes(part, "text", content.String())
							template, _ = sjson.SetRawBytes(template, "candidates.0.content.parts.-1", part)
						}

					case "message":
						// Flush any pending function calls before adding non-function content
						flushPendingFunctionCalls()

						// Add regular text content
						if content := value.Get("content"); content.Exists() && content.IsArray() {
							content.ForEach(
								func(_, contentItem gjson.Result) bool {
									if contentItem.Get("type").String() == "output_text" {
										if text := contentItem.Get("text"); text.Exists() {
											part := []byte(`{"text":""}`)
											part, _ = sjson.SetBytes(part, "text", text.String())
											template, _ = sjson.SetRawBytes(
												template, "candidates.0.content.parts.-1", part,
											)
										}
									}
									return true
								},
							)
						}

					case "image_generation_call":
						flushPendingFunctionCalls()
						if image := codexGeminiImage(value, "result"); len(image) > 0 {
							template, _ = sjson.SetRawBytes(template, "candidates.0.content.parts.-1", image)
						}

					case "function_call":
						// Collect function call for potential merging with consecutive ones
						functionCall := []byte(`{"functionCall":{"args":{},"name":""}}`)
						if id := value.Get("call_id").String(); id != "" {
							functionCall, _ = sjson.SetBytes(functionCall, "functionCall.id", id)
						}
						{
							n := value.Get("name").String()
							rev := buildReverseMapFromGeminiOriginal(originalRequestRawJSON)
							if orig, ok := rev[n]; ok {
								n = orig
							}
							functionCall, _ = sjson.SetBytes(functionCall, "functionCall.name", n)
						}

						// Parse and set arguments
						if argsStr := value.Get("arguments").String(); argsStr != "" {
							argsResult := gjson.Parse(argsStr)
							if argsResult.IsObject() {
								functionCall, _ = sjson.SetRawBytes(
									functionCall,
									"functionCall.args",
									[]byte(argsStr),
								)
							}
						}

						pendingFunctionCalls = append(pendingFunctionCalls, functionCall)
					}
					return true
				},
			)

			// Handle any remaining pending function calls at the end
			flushPendingFunctionCalls()
		}

	}
	return template
}

func codexGeminiIncompleteFinishReason(reason string) string {
	switch reason {
	case "max_tokens", "max_output_tokens":
		return "MAX_TOKENS"
	case "content_filter":
		return "SAFETY"
	default:
		return "OTHER"
	}
}

// buildReverseMapFromGeminiOriginal builds a map[short]original from original Gemini request tools.
func buildReverseMapFromGeminiOriginal(original []byte) map[string]string {
	tools := gjson.GetBytes(original, "tools")
	rev := map[string]string{}
	if !tools.IsArray() {
		return rev
	}
	var names []string
	toolArray := tools.Array()
	for i := range toolArray {
		fns := toolArray[i].Get("functionDeclarations")
		if !fns.IsArray() {
			continue
		}
		for _, fn := range fns.Array() {
			if v := fn.Get("name"); v.Exists() {
				names = append(names, v.String())
			}
		}
	}
	if len(names) > 0 {
		m := buildShortNameMap(names)
		for orig, short := range m {
			rev[short] = orig
		}
	}
	return rev
}

func tokenCount(_ context.Context, count int64) []byte {
	return translatorcommon.GeminiTokenCountJSON(count)
}

func codexGeminiImage(item gjson.Result, field string) []byte {
	data := item.Get(field)
	if data.Type != gjson.String || strings.TrimSpace(data.String()) == "" {
		return nil
	}
	format := strings.ToLower(strings.TrimSpace(item.Get("output_format").String()))
	switch format {
	case "":
		format = "png"
	case "jpg":
		format = "jpeg"
	case "png", "jpeg", "webp":
	default:
		log.WithField("output_format", format).Warn("Ignoring unsupported Codex image format")
		return nil
	}
	part := []byte(`{"inlineData":{"mimeType":"","data":""}}`)
	part, _ = sjson.SetBytes(part, "inlineData.mimeType", "image/"+format)
	part, _ = sjson.SetBytes(part, "inlineData.data", strings.TrimSpace(data.String()))
	return part
}
