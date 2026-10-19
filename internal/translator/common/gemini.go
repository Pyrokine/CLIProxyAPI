package common

import (
	"strings"

	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ReorderGeminiUserParts places trailing text before function responses within
// one user turn. Image and other part slots are left untouched.
func ReorderGeminiUserParts(parts [][]byte) [][]byte {
	seenFunctionResponse := false
	hasTrailingText := false
	textParts := make([][]byte, 0, len(parts))
	functionResponses := make([][]byte, 0, len(parts))
	reorderSlots := make([]int, 0, len(parts))

	for index, part := range parts {
		isText := gjson.GetBytes(part, "text").Exists()
		isFunctionResponse := geminiFunctionResponsePart(part)
		if !isText && !isFunctionResponse {
			continue
		}
		reorderSlots = append(reorderSlots, index)
		if isText {
			textParts = append(textParts, part)
			if seenFunctionResponse {
				hasTrailingText = true
			}
			continue
		}
		seenFunctionResponse = true
		functionResponses = append(functionResponses, part)
	}
	if !hasTrailingText {
		return parts
	}

	ordered := append([][]byte(nil), parts...)
	reordered := append(textParts, functionResponses...)
	for index, slot := range reorderSlots {
		ordered[slot] = reordered[index]
	}
	return ordered
}

// MergeAdjacentGeminiContents merges consecutive user turns at their existing
// user boundary. Function responses remain in result slots, trailing text is
// placed before them, and image or other part slots are not moved.
func MergeAdjacentGeminiContents(contents [][]byte) [][]byte {
	return mergeAdjacentGeminiContents(contents, false)
}

// MergeAdjacentGeminiUserContents merges consecutive plain user turns while
// preserving every function-response boundary and all part order within a turn.
func MergeAdjacentGeminiUserContents(contents [][]byte) [][]byte {
	return mergeAdjacentGeminiContents(contents, true)
}

func mergeAdjacentGeminiContents(contents [][]byte, preserveFunctionResponseBoundary bool) [][]byte {
	if len(contents) <= 1 {
		return contents
	}

	merged := make([][]byte, 0, len(contents))
	for _, content := range contents {
		if !validGeminiContent(content) {
			continue
		}
		if len(merged) == 0 || gjson.GetBytes(content, "role").String() != "user" {
			merged = append(merged, content)
			continue
		}

		lastIndex := len(merged) - 1
		last := merged[lastIndex]
		if gjson.GetBytes(last, "role").String() != "user" ||
			preserveFunctionResponseBoundary &&
				(ContentHasGeminiFunctionResponse(last) || ContentHasGeminiFunctionResponse(content)) {
			merged = append(merged, content)
			continue
		}

		parts := append(geminiContentParts(last), geminiContentParts(content)...)
		if !preserveFunctionResponseBoundary {
			parts = ReorderGeminiUserParts(parts)
		}
		updated, err := sjson.SetRawBytes(last, "parts", JoinRawArray(parts))
		if err != nil {
			merged = append(merged, content)
			continue
		}
		merged[lastIndex] = updated
	}
	return merged
}

func geminiFunctionResponsePart(part []byte) bool {
	return gjson.GetBytes(part, "functionResponse").Exists() ||
		gjson.GetBytes(part, "function_response").Exists()
}

func validGeminiContent(content []byte) bool {
	if len(content) == 0 {
		return false
	}
	parts := gjson.GetBytes(content, "parts")
	return parts.IsArray() && len(parts.Array()) > 0
}

func geminiContentParts(content []byte) [][]byte {
	parts := gjson.GetBytes(content, "parts").Array()
	rawParts := make([][]byte, 0, len(parts))
	for _, part := range parts {
		rawParts = append(rawParts, []byte(part.Raw))
	}
	return rawParts
}

// ContentHasGeminiFunctionCall reports whether a content turn contains a tool call.
func ContentHasGeminiFunctionCall(content []byte) bool {
	for _, part := range gjson.GetBytes(content, "parts").Array() {
		if part.Get("functionCall").Exists() || part.Get("function_call").Exists() {
			return true
		}
	}
	return false
}

// SplitGeminiFunctionResponseTurns keeps tool results adjacent to their preceding model call.
func SplitGeminiFunctionResponseTurns(contents [][]byte) [][]byte {
	if len(contents) == 0 {
		return contents
	}

	split := make([][]byte, 0, len(contents))
	for _, content := range contents {
		if gjson.GetBytes(content, "role").String() != "user" || !ContentHasGeminiFunctionResponse(content) {
			split = append(split, content)
			continue
		}
		var responses, other [][]byte
		for _, part := range gjson.GetBytes(content, "parts").Array() {
			if part.Get("functionResponse").Exists() || part.Get("function_response").Exists() {
				responses = append(responses, []byte(part.Raw))
			} else {
				other = append(other, []byte(part.Raw))
			}
		}
		if len(other) == 0 {
			split = append(split, content)
			continue
		}
		responseTurn, responseErr := sjson.SetRawBytes(content, "parts", JoinRawArray(responses))
		otherTurn, otherErr := sjson.SetRawBytes(content, "parts", JoinRawArray(other))
		if responseErr != nil || otherErr != nil {
			log.WithFields(log.Fields{"response_error": responseErr, "other_error": otherErr}).Warn("Gemini tool response turn could not be separated")
			split = append(split, content)
			continue
		}
		split = append(split, responseTurn, otherTurn)
	}

	ordered := make([][]byte, 0, len(split))
	for index := 0; index < len(split); {
		if gjson.GetBytes(split[index], "role").String() != "user" {
			ordered = append(ordered, split[index])
			index++
			continue
		}
		previousModelCalledTool := len(ordered) > 0 &&
			gjson.GetBytes(ordered[len(ordered)-1], "role").String() == "model" &&
			ContentHasGeminiFunctionCall(ordered[len(ordered)-1])
		end := index
		for end < len(split) && gjson.GetBytes(split[end], "role").String() == "user" {
			end++
		}
		if !previousModelCalledTool {
			ordered = append(ordered, split[index:end]...)
			index = end
			continue
		}
		var responses, other [][]byte
		var responseTurn []byte
		for _, turn := range split[index:end] {
			if ContentHasGeminiFunctionResponse(turn) {
				if responseTurn == nil {
					responseTurn = turn
				}
				responses = append(responses, geminiContentParts(turn)...)
			} else {
				other = append(other, turn)
			}
		}
		if len(responses) > 0 && end-index > 1 {
			combined, err := sjson.SetRawBytes(responseTurn, "parts", JoinRawArray(responses))
			if err != nil {
				log.WithError(err).Warn("Gemini tool responses could not be combined")
				ordered = append(ordered, split[index:end]...)
			} else {
				ordered = append(ordered, combined)
				ordered = append(ordered, other...)
			}
		} else {
			ordered = append(ordered, split[index:end]...)
		}
		index = end
	}
	return ordered
}

// ContainsJSONRef reports whether a schema reference appears in a nested function result.
func ContainsJSONRef(value gjson.Result) bool {
	if !value.IsObject() && !value.IsArray() {
		return false
	}
	found := false
	value.ForEach(func(key, child gjson.Result) bool {
		if value.IsObject() && key.String() == "$ref" && child.Type == gjson.String {
			found = true
			return false
		}
		if ContainsJSONRef(child) {
			found = true
			return false
		}
		return true
	})
	return found
}

// SetGeminiFunctionResponseResult keeps JSON Schema references opaque to Gemini's media parser.
func SetGeminiFunctionResponseResult(part []byte, path string, result gjson.Result) []byte {
	if !result.Exists() {
		part, _ = sjson.SetBytes(part, path, "")
		return part
	}
	if ContainsJSONRef(result) {
		target := path
		if strings.HasSuffix(path, "response") {
			target += ".result"
		}
		part, _ = sjson.SetBytes(part, target, result.Raw)
		return part
	}
	part, _ = sjson.SetRawBytes(part, path, []byte(result.Raw))
	return part
}

// SetGeminiFunctionResponseRaw converts a raw tool result using the same reference handling.
func SetGeminiFunctionResponseRaw(part []byte, path string, rawJSON string) []byte {
	if strings.TrimSpace(rawJSON) == "" {
		part, _ = sjson.SetBytes(part, path, "")
		return part
	}
	return SetGeminiFunctionResponseResult(part, path, gjson.Parse(rawJSON))
}
