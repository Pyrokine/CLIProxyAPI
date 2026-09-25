package common

import (
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
