// Package common provides shared response translation helpers for Gemini CLI API variants.
package common

import (
	"fmt"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// functionCallIDCounter provides a process-wide unique counter for function call identifiers
// shared across all Gemini response converters.
var functionCallIDCounter uint64

// sSEChunkTemplate is the base OpenAI SSE streaming response template.
const sSEChunkTemplate = `{"id":"","object":"chat.completion.chunk","created":12345,"model":"model","choices":[{"index":0,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":null},"finish_reason":null,"native_finish_reason":null}]}`

// CLIChunkResult holds the processed results from a Gemini CLI streaming response chunk.
type CLIChunkResult struct {
	Template    string
	HasToolCall bool
}

// ProcessCLIResponseChunk processes the common parts of a Gemini CLI streaming response chunk:
// model version, creation timestamp, response ID, usage metadata, and content parts (text,
// functionCall, inlineData). The finish_reason handling is left to the caller.
func ProcessCLIResponseChunk(rawJSON []byte, unixTimestamp *int64, functionIndex *int) CLIChunkResult {
	template := sSEChunkTemplate

	// Extract and set the model version.
	if v := gjson.GetBytes(rawJSON, "response.modelVersion"); v.Exists() {
		template, _ = sjson.Set(template, "model", v.String())
	}

	// Extract and set the creation timestamp.
	if ct := gjson.GetBytes(rawJSON, "response.createTime"); ct.Exists() {
		t, err := time.Parse(time.RFC3339Nano, ct.String())
		if err == nil {
			*unixTimestamp = t.Unix()
		}
	}
	template, _ = sjson.Set(template, "created", *unixTimestamp)

	// Extract and set the response ID.
	if id := gjson.GetBytes(rawJSON, "response.responseId"); id.Exists() {
		template, _ = sjson.Set(template, "id", id.String())
	}

	// Extract and set usage metadata (token counts).
	if usage := gjson.GetBytes(rawJSON, "response.usageMetadata"); usage.Exists() {
		template = SetUsageMetadata(template, usage)
	}

	// Process content parts.
	prefix := "choices.0.delta"
	hasToolCall := false
	partsResult := gjson.GetBytes(rawJSON, "response.candidates.0.content.parts")
	if partsResult.IsArray() {
		for _, part := range partsResult.Array() {
			skip, isFunctionCall, updated := ProcessContentPart(template, part, prefix, functionIndex)
			if skip {
				continue
			}
			template = updated
			if isFunctionCall {
				hasToolCall = true
			}
		}
	}

	return CLIChunkResult{
		Template:    template,
		HasToolCall: hasToolCall,
	}
}

// AppendFunctionCall appends a Gemini function call to the template's tool_calls array at the given prefix.
// The prefix determines the JSON path (e.g., "choices.0.delta" for streaming, "message" for non-streaming).
// functionIndex is a running counter that tracks tool call indices; callers may pass nil for non-streaming
// responses where indexing is not needed.
func AppendFunctionCall(
	template string, functionCallResult gjson.Result, prefix string, functionIndex *int,
) string {
	toolCallsPath := prefix + ".tool_calls"
	toolCallsResult := gjson.Get(template, toolCallsPath)

	idx := 0
	if functionIndex != nil {
		idx = *functionIndex
		*functionIndex++
	}
	if toolCallsResult.Exists() && toolCallsResult.IsArray() {
		idx = len(toolCallsResult.Array())
	} else {
		template, _ = sjson.SetRaw(template, toolCallsPath, `[]`)
	}

	fc := `{"id": "","index": 0,"type": "function","function": {"name": "","arguments": ""}}`
	fcName := functionCallResult.Get("name").String()
	fc, _ = sjson.Set(
		fc, "id",
		fmt.Sprintf("%s-%d-%d", fcName, time.Now().UnixNano(), atomic.AddUint64(&functionCallIDCounter, 1)),
	)
	fc, _ = sjson.Set(fc, "index", idx)
	fc, _ = sjson.Set(fc, "function.name", fcName)
	if fcArgsResult := functionCallResult.Get("args"); fcArgsResult.Exists() {
		fc, _ = sjson.Set(fc, "function.arguments", fcArgsResult.Raw)
	}
	template, _ = sjson.Set(template, prefix+".role", "assistant")
	template, _ = sjson.SetRaw(template, toolCallsPath+".-1", fc)
	return template
}

// AppendInlineData processes a Gemini inlineData part and appends it to the template's images array
// at the given prefix.
func AppendInlineData(template string, inlineDataResult gjson.Result, prefix string) (string, bool) {
	data := inlineDataResult.Get("data").String()
	if data == "" {
		return template, false
	}
	mimeType := inlineDataResult.Get("mimeType").String()
	if mimeType == "" {
		mimeType = inlineDataResult.Get("mime_type").String()
	}
	if mimeType == "" {
		mimeType = "image/png"
	}
	imageURL := fmt.Sprintf("data:%s;base64,%s", mimeType, data)
	imagesPath := prefix + ".images"
	imagesResult := gjson.Get(template, imagesPath)
	if !imagesResult.Exists() || !imagesResult.IsArray() {
		template, _ = sjson.SetRaw(template, imagesPath, `[]`)
	}
	imageIndex := len(gjson.Get(template, imagesPath).Array())
	imagePayload := `{"type":"image_url","image_url":{"url":""}}`
	imagePayload, _ = sjson.Set(imagePayload, "index", imageIndex)
	imagePayload, _ = sjson.Set(imagePayload, "image_url.url", imageURL)
	template, _ = sjson.Set(template, prefix+".role", "assistant")
	template, _ = sjson.SetRaw(template, imagesPath+".-1", imagePayload)
	return template, true
}

// SetTimestamp extracts a timestamp field from rawJSON, parses it as RFC3339Nano,
// stores the Unix timestamp into the provided pointer, and sets it on the template.
func SetTimestamp(rawJSON []byte, unixTimestamp *int64, template string, field string) string {
	if ct := gjson.GetBytes(rawJSON, field); ct.Exists() {
		t, err := time.Parse(time.RFC3339Nano, ct.String())
		if err == nil {
			*unixTimestamp = t.Unix()
		}
	}
	template, _ = sjson.Set(template, "created", *unixTimestamp)
	return template
}

// ProcessContentPart processes a single Gemini content part (text, functionCall, inlineData)
// against the given template and prefix. It handles thoughtSignature filtering.
//
// Returns:
//   - skip: true if this part should be skipped (pure thoughtSignature)
//   - isFunctionCall: true if this part is a function call
//   - updated: the updated template string
func ProcessContentPart(
	template string, part gjson.Result, prefix string, functionIndex *int,
) (skip bool, isFunctionCall bool, updated string) {
	partTextResult := part.Get("text")
	functionCallResult := part.Get("functionCall")
	thoughtSignatureResult := part.Get("thoughtSignature")
	if !thoughtSignatureResult.Exists() {
		thoughtSignatureResult = part.Get("thought_signature")
	}
	inlineDataResult := part.Get("inlineData")
	if !inlineDataResult.Exists() {
		inlineDataResult = part.Get("inline_data")
	}

	hasThoughtSignature := thoughtSignatureResult.Exists() && thoughtSignatureResult.String() != ""
	hasContentPayload := partTextResult.Exists() || functionCallResult.Exists() || inlineDataResult.Exists()

	// Skip pure thoughtSignature parts but keep any actual payload in the same part.
	if hasThoughtSignature && !hasContentPayload {
		return true, false, template
	}

	if partTextResult.Exists() {
		text := partTextResult.String()
		if part.Get("thought").Bool() {
			template, _ = sjson.Set(template, prefix+".reasoning_content", text)
		} else {
			template, _ = sjson.Set(template, prefix+".content", text)
		}
		template, _ = sjson.Set(template, prefix+".role", "assistant")
		return false, false, template
	}

	if functionCallResult.Exists() {
		template = AppendFunctionCall(template, functionCallResult, prefix, functionIndex)
		return false, true, template
	}

	if inlineDataResult.Exists() {
		if result, ok := AppendInlineData(template, inlineDataResult, prefix); ok {
			template = result
		}
		return false, false, template
	}

	return false, false, template
}

// SetUsageMetadata applies Gemini token usage information to the template.
func SetUsageMetadata(template string, usage gjson.Result) string {
	cachedTokenCount := usage.Get("cachedContentTokenCount").Int()
	if v := usage.Get("candidatesTokenCount"); v.Exists() {
		template, _ = sjson.Set(template, "usage.completion_tokens", v.Int())
	}
	if v := usage.Get("totalTokenCount"); v.Exists() {
		template, _ = sjson.Set(template, "usage.total_tokens", v.Int())
	}
	promptTokenCount := usage.Get("promptTokenCount").Int()
	thoughtsTokenCount := usage.Get("thoughtsTokenCount").Int()
	template, _ = sjson.Set(template, "usage.prompt_tokens", promptTokenCount)
	if thoughtsTokenCount > 0 {
		template, _ = sjson.Set(template, "usage.completion_tokens_details.reasoning_tokens", thoughtsTokenCount)
	}
	if cachedTokenCount > 0 {
		var err error
		template, err = sjson.Set(template, "usage.prompt_tokens_details.cached_tokens", cachedTokenCount)
		if err != nil {
			log.Warnf("gemini response: failed to set cached_tokens: %v", err)
		}
	}
	return template
}
